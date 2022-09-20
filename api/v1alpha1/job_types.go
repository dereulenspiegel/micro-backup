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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// JobSpec defines the desired state of Job
type JobSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Which PVC to select based on labels
	Selector *metav1.LabelSelector `json:"selector"`
	// +optional
	KeepFailed *bool `json:"keepFailed,omitempty"`
	// +optional
	KeepSuccessful *bool  `json:"keepSuccessful,omitempty"`
	RepoName       string `json:"repoName"`
	// +optional
	Forget *Forget `json:"forget,omitempty"`
}

type Forget struct {
	// +optional
	Prune *bool `json:"prune,omitempty"`
	// +optional
	KeepLast *int `json:"keepLast,omitempty"`
	// +optional
	KeepHourly *int `json:"keepHourly,omitempty"`
	// +optional
	KeepDaily *int `json:"keepDaily,omitempty"`
	// +optional
	KeepWeekly *int `json:"keepWeekly,omitempty"`
	// +optional
	KeepMonthly *int `json:"keepMonthly,omitempty"`
	// +optional
	KeepYearly *int `json:"keepYearly,omitempty"`
}

// JobStatus defines the observed state of Job
type JobStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	Running bool `json:"running"`
	// +optional
	ActiveJobs []corev1.ObjectReference `json:"activeJobs,omitempty"`
	// +optional
	FailedJobs []corev1.ObjectReference `json:"failedJobs,omitempty"`
	// +optional
	SuccessfulJobs []corev1.ObjectReference `json:"successfulJobs,omitempty"`
	// +optional
	BackedUp []corev1.ObjectReference `json:"backedUp,omitempty"`
	// +optional
	WaitingForBackup []corev1.ObjectReference `json:"waitingForBackup,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Job is the Schema for the jobs API
type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JobSpec   `json:"spec,omitempty"`
	Status JobStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// JobList contains a list of Job
type JobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Job `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Job{}, &JobList{})
}
