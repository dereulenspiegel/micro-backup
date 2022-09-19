/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron"

	"k8s.io/apimachinery/pkg/runtime"
	ref "k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	batch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	backupv1alpha1 "github.com/dereulenspiegel/micro-backup/api/v1alpha1"
)

var (
	jobOwnerKey             = ".metadata.controller"
	apiGVStr                = backupv1alpha1.GroupVersion.String()
	scheduledTimeAnnotation = "backup.k8s.akuz.de/scheduled-at"
	// /var/snap/microk8s/common/var/lib/kubelet/pods/1c020433-11b9-4d8b-9d45-53994c57fc2d/volumes/kubernetes.io~csi/pvc-3b74c46f-6227-48c4-9fef-10b431d2bfa3/mount/
	pvcPathPattern = "%s/pods/%s/volumes/kubernetes.io~csi/pvc-%s/mount"
)

type JobControllerOptions struct {
	KubeletPath    string
	ContainerImage string
}

type backupTarget struct {
	pod *v1.Pod
	pvc *v1.PersistentVolumeClaim
}

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (_ realClock) Now() time.Time { return time.Now() }

// JobReconciler reconciles a Job object
type JobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock
	opts *JobControllerOptions
}

//+kubebuilder:rbac:groups=backup.k8s.akuz.de,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=backup.k8s.akuz.de,resources=jobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=backup.k8s.akuz.de,resources=jobs/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Job object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *JobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("backupJobName", req.NamespacedName)
	var backupJob backupv1alpha1.Job
	if err := r.Get(ctx, req.NamespacedName, &backupJob); err != nil {
		logger.Error(err, "failed to find backup job")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var childJobs batch.JobList
	if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		logger.Error(err, "failed to list potential job childs for backup job")
		return ctrl.Result{}, err
	}

	var activeJobs []*batch.Job
	var successfulJobs []*batch.Job
	var failedJobs []*batch.Job

	isJobFinished := func(job *batch.Job) (bool, batch.JobConditionType) {
		for _, c := range job.Status.Conditions {
			if (c.Type == batch.JobComplete || c.Type == batch.JobFailed) && c.Status == corev1.ConditionTrue {
				return true, c.Type
			}
		}

		return false, ""
	}

	for i, job := range childJobs.Items {
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case "": // ongoing
			activeJobs = append(activeJobs, &childJobs.Items[i])
		case batch.JobFailed:
			failedJobs = append(failedJobs, &childJobs.Items[i])
		case batch.JobComplete:
			successfulJobs = append(successfulJobs, &childJobs.Items[i])
		}
	}

	missedRun, nextRun, err := r.getNextSchedule(&backupJob, r.Now())
	if err != nil {
		logger.Error(err, "unable to figure out CronJob schedule", "schedule", backupJob.Spec.Schedule)
		return ctrl.Result{}, nil
	}

	scheduledResult := ctrl.Result{RequeueAfter: nextRun.Sub(r.Now())} // save this so we can re-use it elsewhere

	if !backupJob.Status.Running {
		if len(activeJobs) > 0 {
			logger.Error(errors.New("not running, but jobs active"), "backup job is not running, but there are active jobs", "activeJobs", len(activeJobs))
		}

		if missedRun.IsZero() {
			logger.Info("no upcoming scheduled times, sleeping until next")
			return scheduledResult, nil
		} else {
			backupJob.Status.Running = true
		}
	}

	if backupJob.Spec.Suspend != nil && *backupJob.Spec.Suspend {
		logger.Info("backup job is suspended")
		return ctrl.Result{}, nil
	}
	backupJob.Status.Running = true

	var backupTargets []backupTarget
	if len(backupJob.Status.WaitingForBackup) == 0 && len(backupJob.Status.BackedUp) == 0 {
		// Job seems to have just started
		var err error
		backupTargets, err = r.discoverBackupTargets(ctx, logger, req, &backupJob)
		if err != nil {
			logger.Error(err, "failed to query backup targets")
			return ctrl.Result{Requeue: true}, err
		}
		for _, backupTarget := range backupTargets {
			pvcRef, err := ref.GetReference(r.Scheme, backupTarget.pvc)
			if err != nil {
				logger.Error(err, "couldn't create reference for pvc", "pvc", backupTarget.pvc)
				continue
			}
			backupJob.Status.WaitingForBackup = append(backupJob.Status.WaitingForBackup, *pvcRef)
		}
	}

	backupJob.Status.ActiveJobs = nil
	for _, activeJob := range activeJobs {
		jobRef, err := ref.GetReference(r.Scheme, activeJob)
		if err != nil {
			logger.Error(err, "unable to make reference to active job", "job", activeJob)
			continue
		}
		backupJob.Status.ActiveJobs = append(backupJob.Status.ActiveJobs, *jobRef)
	}

	for _, failedJob := range failedJobs {
		jobRef, err := ref.GetReference(r.Scheme, failedJob)
		if err != nil {
			logger.Error(err, "unable to make reference to failed job", "job", failedJob)
			continue
		}
		backupJob.Status.FailedJobs = append(backupJob.Status.FailedJobs, *jobRef)
	}
	logger.Info("job count", "activeJobs", len(activeJobs), "successfulJobs", len(successfulJobs), "failedJobs", len(failedJobs))

	for _, successfulJob := range successfulJobs {
		for _, volume := range successfulJob.Spec.Template.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				var pvc v1.PersistentVolumeClaim
				if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: volume.PersistentVolumeClaim.ClaimName}, &pvc); err != nil {
					logger.Error(err, "failed to query pvc from successful backup job", "job", successfulJob)
					continue
				}
				pvcRef, err := ref.GetReference(r.Scheme, &pvc)
				if err != nil {
					logger.Error(err, "failed to create reference to successfully backed up pvc", "pvc", pvc)
					continue
				}
				backupJob.Status.BackedUp = append(backupJob.Status.BackedUp, *pvcRef)
				break
			}
		}
		if err := r.Delete(ctx, successfulJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			logger.Error(err, "failed to delete successful job", "job", successfulJob)
		}
	}

	for i, bt := range backupTargets {
		for _, backedUpRef := range backupJob.Status.BackedUp {
			if backedUpRef.Name == bt.pvc.Name {
				backupTargets = remove(backupTargets, i)
			}
		}
	}

	for i, pvcRef := range backupJob.Status.WaitingForBackup {
		for _, backedUpRef := range backupJob.Status.BackedUp {
			if pvcRef.Name == backedUpRef.Name {
				backupJob.Status.WaitingForBackup = remove(backupJob.Status.WaitingForBackup, i)
				break
			}
		}
	}

	if len(activeJobs) > 0 {
		logger.Info("skipping current backup job execution as another job is still running", "activeJobs", len(activeJobs))
		return ctrl.Result{Requeue: true}, nil
	}

	if len(backupTargets) == 0 {
		if err := r.Status().Update(ctx, &backupJob); err != nil {
			logger.Error(err, "unable to update BackupJob status")
			return ctrl.Result{}, err
		}
		backupJob.Status.Running = false
		return scheduledResult, nil
	}

	batchJob, err := r.constructBackupJob(ctx, logger, &backupJob, backupTargets[0])
	if err := r.Create(ctx, batchJob); err != nil {
		logger.Error(err, "failed to create batch job")
		return ctrl.Result{}, err
	}

	if err := r.Get(ctx, types.NamespacedName{Name: batchJob.Name, Namespace: batchJob.Namespace}, batchJob); err != nil {
		logger.Error(err, "failed to query freshly created batch job")
		return ctrl.Result{}, err
	}

	batchJobRef, err := ref.GetReference(r.Scheme, batchJob)
	if err != nil {
		logger.Error(err, "failed to create reference to batch job", "job", batchJob)
		return ctrl.Result{}, err
	}
	backupJob.Status.ActiveJobs = append(backupJob.Status.ActiveJobs, *batchJobRef)

	if err := r.Status().Update(ctx, &backupJob); err != nil {
		logger.Error(err, "unable to update BackupJob status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *JobReconciler) getNextSchedule(backupJob *backupv1alpha1.Job, now time.Time) (lastMissed time.Time, next time.Time, err error) {
	sched, err := cron.ParseStandard(backupJob.Spec.Schedule)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("unparseable schedule %q: %v", backupJob.Spec.Schedule, err)
	}

	var earliestTime time.Time
	if backupJob.Status.LastScheduleTime != nil {
		earliestTime = backupJob.Status.LastScheduleTime.Time
	} else {
		earliestTime = backupJob.ObjectMeta.CreationTimestamp.Time
	}
	if earliestTime.After(now) {
		return time.Time{}, sched.Next(now), nil
	}

	starts := 0
	for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
		lastMissed = t
		starts++
		if starts > 100 {
			// We can't get the most recent times so just return an empty slice
			return time.Time{}, time.Time{}, fmt.Errorf("Too many missed start times (> 100).")
		}
	}
	return lastMissed, sched.Next(now), nil
}

func (r *JobReconciler) discoverBackupTargets(ctx context.Context, logger logr.Logger, req ctrl.Request, backupJob *backupv1alpha1.Job) (backupTargets []backupTarget, err error) {
	var pvcList v1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcList, client.InNamespace(req.Namespace), client.MatchingLabels(backupJob.Spec.Selector.MatchLabels)); err != nil {
		logger.Error(err, "failed to list pvcs to backup")
		return backupTargets, err
	}
	var podList v1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(req.Namespace)); err != nil {
		logger.Error(err, "failed to list pods in namespace")
		return backupTargets, err
	}

	for _, pvc := range pvcList.Items {
		var connectedPod *v1.Pod
		for _, pod := range podList.Items {
			for _, volume := range pod.Spec.Volumes {
				if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvc.Name {
					connectedPod = &pod
					break
				}
			}
			if connectedPod != nil {
				break
			}
		}
		if connectedPod != nil {
			backupTargets = append(backupTargets, backupTarget{
				pod: connectedPod,
				pvc: &pvc,
			})
			connectedPod = nil
		} else {
			logger.Error(errors.New("pvc without connected pod"), "found pvc without connected pod", "pvc", pvc)
		}
	}
	return
}

func (r *JobReconciler) constructBackupJob(ctx context.Context, logger logr.Logger, backupJob *backupv1alpha1.Job, bt backupTarget) (*batch.Job, error) {
	name := fmt.Sprintf("%s-%s-%d", backupJob.Name, bt.pvc.Name, time.Now().Unix())

	pvcHostPath := fmt.Sprintf(pvcPathPattern, r.opts.KubeletPath, bt.pod.ObjectMeta.UID, bt.pvc.ObjectMeta.UID)
	backoffLimit := int32(1)
	runAsNoNRoot := true
	job := &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"backupJob": backupJob.Name,
				"pvc":       bt.pvc.Name,
			},
			Annotations: map[string]string{},
			Namespace:   backupJob.Namespace,
		},
		Spec: batch.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: name,
					Namespace:    backupJob.Namespace,
					Labels: map[string]string{
						"backupJob": backupJob.Name,
						"pvc":       bt.pvc.Name,
					},
					Annotations: map[string]string{},
				},
				Spec: v1.PodSpec{
					NodeName: bt.pod.Spec.NodeName,
					Volumes: []v1.Volume{
						{
							Name: "targetPvc",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: pvcHostPath,
								},
							},
						},
					},
					SecurityContext: &v1.PodSecurityContext{
						RunAsNonRoot: &runAsNoNRoot,
					},
					Containers: []v1.Container{
						{
							Name:    "restic",
							Image:   r.opts.ContainerImage,
							Command: []string{"tbd"},
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "targetPvc",
									ReadOnly:  true,
									MountPath: "/targetPvc",
								},
							},
							SecurityContext: &v1.SecurityContext{
								RunAsNonRoot: &runAsNoNRoot,
								Capabilities: &v1.Capabilities{
									Add: []v1.Capability{
										"DAC_READ_SEARCH",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(backupJob, job, r.Scheme); err != nil {
		logger.Error(err, "failed to set controller reference for job")
		return nil, err
	}
	return job, nil
}

// func (r *JobReconciler) constructJobForCronJob(cronJob *batchv1.CronJob, scheduledTime time.Time) (*kbatch.Job, error) {
// 	// We want job names for a given nominal start time to have a deterministic name to avoid the same job being created twice
// 	name := fmt.Sprintf("%s-%d", cronJob.Name, scheduledTime.Unix())

// 	job := &kbatch.Job{
// 		ObjectMeta: metav1.ObjectMeta{
// 			Labels:      make(map[string]string),
// 			Annotations: make(map[string]string),
// 			Name:        name,
// 			Namespace:   cronJob.Namespace,
// 		},
// 		Spec: *cronJob.Spec.JobTemplate.Spec.DeepCopy(),
// 	}
// 	for k, v := range cronJob.Spec.JobTemplate.Annotations {
// 		job.Annotations[k] = v
// 	}
// 	job.Annotations[scheduledTimeAnnotation] = scheduledTime.Format(time.RFC3339)
// 	for k, v := range cronJob.Spec.JobTemplate.Labels {
// 		job.Labels[k] = v
// 	}
// 	if err := ctrl.SetControllerReference(cronJob, job, r.Scheme); err != nil {
// 		return nil, err
// 	}

// 	return job, nil
// }

// SetupWithManager sets up the controller with the Manager.
func (r *JobReconciler) SetupWithManager(mgr ctrl.Manager, opts *JobControllerOptions) error {
	if r.Clock == nil {
		r.Clock = realClock{}
	}
	r.opts = opts

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &batch.Job{}, jobOwnerKey, func(rawObj client.Object) []string {
		job := rawObj.(*batch.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		if owner.APIVersion != apiGVStr || owner.Kind != "Job" {
			return nil
		}

		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&backupv1alpha1.Job{}).
		Owns(&batch.Job{}).
		Complete(r)
}

func remove[T any](sl []T, i int) []T {
	sl[i] = sl[len(sl)-1]
	return sl[:len(sl)-1]
}
