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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ref "k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	batch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backupv1alpha1 "github.com/dereulenspiegel/micro-backup/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/robfig/cron"
)

type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (_ realClock) Now() time.Time { return time.Now() }

// ScheduleReconciler reconciles a Schedule object
type ScheduleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock
}

//+kubebuilder:rbac:groups=backup.k8s.akuz.de,resources=schedules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=backup.k8s.akuz.de,resources=schedules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=backup.k8s.akuz.de,resources=schedules/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Schedule object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *ScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	logger := log.FromContext(ctx).WithValues("backupScheduleName", req.NamespacedName)

	var schedule backupv1alpha1.Schedule
	if err := r.Get(ctx, req.NamespacedName, &schedule); err != nil {
		logger.Error(err, "failed to find backup schedule")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	missedRun, nextRun, err := r.getNextSchedule(&schedule, r.Now())
	if err != nil {
		logger.Error(err, "unable to figure out CronJob schedule", "schedule", schedule.Spec.Schedule)
		return ctrl.Result{}, nil
	}

	schedule.Status.LastScheduleTime = &v1.Time{Time: nextRun}

	scheduledResult := ctrl.Result{RequeueAfter: nextRun.Sub(r.Now())} // save this so we can re-use it elsewhere

	if schedule.Status.CurrentJob != nil {
		var backupJob backupv1alpha1.Job
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: schedule.Status.CurrentJob.Namespace,
			Name:      schedule.Status.CurrentJob.Name}, &backupJob); err != nil {

			logger.Error(err, "failed to query currently running backup job", "jobRef", schedule.Status.CurrentJob)
			return ctrl.Result{}, err
		}
		if backupJob.Status.Running {
			// Wait until backup job finishes
			return ctrl.Result{Requeue: true}, nil
		} else {
			var jobsToDelete []corev1.ObjectReference
			if len(backupJob.Status.FailedJobs) > 0 {
				// Consider job failed
				schedule.Status.FailedJobs, jobsToDelete = appendAndKeep(schedule.Status.FailedJobs, *schedule.Status.CurrentJob, schedule.Spec.KeepLastFailed)
			} else {
				schedule.Status.SuccessfulJobs, jobsToDelete = appendAndKeep(schedule.Status.SuccessfulJobs, *schedule.Status.CurrentJob, schedule.Spec.KeepLastSuccessful)
			}
			if err := r.deleteBackupJobs(ctx, logger, jobsToDelete); err != nil {
				logger.Error(err, "failed to delete backup jobs and child resources")
				return ctrl.Result{}, err
			}
			schedule.Status.CurrentJob = nil
			if err := r.Update(ctx, &schedule); err != nil {
				logger.Error(err, "failed to update backup schedule")
				return ctrl.Result{}, err
			}
		}
	}

	if schedule.Spec.Suspend != nil && *schedule.Spec.Suspend {
		logger.Info("backup schedule is suspended")
		return ctrl.Result{}, nil
	}
	if missedRun.IsZero() {
		logger.Info("no upcoming scheduled times, sleeping until next")
		return scheduledResult, nil
	}
	// We should create a new backup job

	backupJob := &backupv1alpha1.Job{
		Spec: schedule.Spec.Job,
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: schedule.ObjectMeta.Name,
			Namespace:    req.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "micro-backup",
				"app.kubernetes.io/instance":   schedule.Name,
			},
			Annotations: map[string]string{
				"backup.k8s.akuz.de/scheduledAt": r.Now().Format(time.RFC3339),
			},
		},
	}

	if err := r.Create(ctx, backupJob); err != nil {
		logger.Error(err, "failed to create backup job")
		return ctrl.Result{}, err
	} else {
		jobRef, err := ref.GetReference(r.Scheme, backupJob)
		if err != nil {
			logger.Error(err, "failed to create reference to backup job", "job", backupJob)
			return ctrl.Result{}, err
		}
		schedule.Status.CurrentJob = jobRef
		schedule.Status.LastScheduleTime = &v1.Time{Time: r.Now()}
		if err := r.Update(ctx, &schedule); err != nil {
			logger.Error(err, "failed to update schedule status")
			return ctrl.Result{}, err
		}
		// Wait for the job to complete, will surely take longer than 2 mins
		return ctrl.Result{RequeueAfter: time.Minute * 2}, nil
	}
}

func appendAndKeep[T any](sl []T, elem T, keep *int) ([]T, []T) {
	sl = append(sl, elem)
	k := 0
	if keep != nil {
		k = *keep
	}
	var leftover []T
	if len(sl) > k {
		diff := len(sl) - k
		sl = sl[diff:]
		leftover = sl[:diff]
	}
	return sl, leftover
}

func (r *ScheduleReconciler) deleteBackupJobs(ctx context.Context, logger logr.Logger, backupJobRefs []corev1.ObjectReference) error {
	for _, backupJobRef := range backupJobRefs {
		var backupJob backupv1alpha1.Job
		if err := r.Get(ctx, types.NamespacedName{Name: backupJob.Name, Namespace: backupJobRef.Namespace}, &backupJob); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			logger.Error(err, "failed to query backup Job", "backupJobRef", backupJobRef)
			return err
		}

		if err := r.deleteAllJobs(ctx, logger, backupJob.Status.FailedJobs); err != nil {
			logger.Error(err, "failed to delete jobs for backupJobs", "backupJob", backupJob)
			return err
		}
		// TODO delete successful jobs
	}
	return nil
}

func (r *ScheduleReconciler) deleteAllJobs(ctx context.Context, logger logr.Logger, jobRefs []corev1.ObjectReference) error {
	for _, jobRef := range jobRefs {
		var batchJob batch.Job
		if err := r.Get(ctx, types.NamespacedName{Name: jobRef.Name, Namespace: jobRef.Namespace}, &batchJob); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			logger.Error(err, "failed to query batch job", "jobRef", jobRef)
			return err
		}
		if err := r.Delete(ctx, &batchJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			logger.Error(err, "failed to delete job", "job", batchJob)
			return err
		}
	}
	return nil
}

func (r *ScheduleReconciler) getNextSchedule(schedule *backupv1alpha1.Schedule, now time.Time) (lastMissed time.Time, next time.Time, err error) {
	sched, err := cron.ParseStandard(schedule.Spec.Schedule)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("unparseable schedule %q: %v", schedule.Spec.Schedule, err)
	}

	var earliestTime time.Time
	if schedule.Status.LastScheduleTime != nil {
		earliestTime = schedule.Status.LastScheduleTime.Time
	} else {
		earliestTime = schedule.ObjectMeta.CreationTimestamp.Time
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

// SetupWithManager sets up the controller with the Manager.
func (r *ScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = realClock{}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&backupv1alpha1.Schedule{}).
		Complete(r)
}
