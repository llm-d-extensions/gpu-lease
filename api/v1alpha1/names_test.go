/*
Copyright 2026.

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
	"testing"
	"time"
)

func TestLeaseName(t *testing.T) {
	got := LeaseName("kind-wva-gpu-cluster-worker", 2)
	want := "kind-wva-gpu-cluster-worker-gpu-2"
	if got != want {
		t.Errorf("LeaseName() = %q, want %q", got, want)
	}
}

func TestDefaultLocalQueueName(t *testing.T) {
	if got, want := DefaultLocalQueueName("app-a"), "app-a-gpu"; got != want {
		t.Errorf("DefaultLocalQueueName() = %q, want %q", got, want)
	}
}

func TestParseDeploymentSettingsDefaults(t *testing.T) {
	s := ParseDeploymentSettings(nil, "app-a")
	want := DeploymentSettings{
		WarmReplicas:      DefaultWarmReplicas,
		MinHotReplicas:    DefaultMinHotReplicas,
		LocalQueueName:    "app-a-gpu",
		PodCapacityRPS:    DefaultPodCapacityRPS,
		ActivationTimeout: DefaultActivationTimeout,
		AdmissionTimeout:  DefaultAdmissionTimeout,
	}
	if s != want {
		t.Errorf("ParseDeploymentSettings(nil) = %+v, want %+v", s, want)
	}
}

func TestParseDeploymentSettingsOverrides(t *testing.T) {
	annotations := map[string]string{
		AnnotationWarmReplicas:      "3",
		AnnotationMinHotReplicas:    "2",
		AnnotationLocalQueue:        "custom-queue",
		AnnotationPodCapacityRPS:    "25",
		AnnotationActivationTimeout: "45s",
		AnnotationAdmissionTimeout:  "10s",
	}
	s := ParseDeploymentSettings(annotations, "app-a")
	want := DeploymentSettings{
		WarmReplicas:      3,
		MinHotReplicas:    2,
		LocalQueueName:    "custom-queue",
		PodCapacityRPS:    25,
		ActivationTimeout: 45 * time.Second,
		AdmissionTimeout:  10 * time.Second,
	}
	if s != want {
		t.Errorf("ParseDeploymentSettings(overrides) = %+v, want %+v", s, want)
	}
}

// TestParseDeploymentSettingsMalformedFallsBackToDefault is the load-bearing test: a
// typo'd annotation must never wedge the controller, it must silently fall back to the
// documented default (names.go's ParseDeploymentSettings doc comment).
func TestParseDeploymentSettingsMalformedFallsBackToDefault(t *testing.T) {
	annotations := map[string]string{
		AnnotationWarmReplicas:      "not-a-number",
		AnnotationMinHotReplicas:    "-1", // negative is also rejected
		AnnotationLocalQueue:        "",   // empty is treated as unset
		AnnotationPodCapacityRPS:    "0",  // zero is rejected (must be > 0)
		AnnotationActivationTimeout: "banana",
		AnnotationAdmissionTimeout:  "-5s", // negative duration rejected
	}
	s := ParseDeploymentSettings(annotations, "app-b")
	want := DeploymentSettings{
		WarmReplicas:      DefaultWarmReplicas,
		MinHotReplicas:    DefaultMinHotReplicas,
		LocalQueueName:    "app-b-gpu",
		PodCapacityRPS:    DefaultPodCapacityRPS,
		ActivationTimeout: DefaultActivationTimeout,
		AdmissionTimeout:  DefaultAdmissionTimeout,
	}
	if s != want {
		t.Errorf("ParseDeploymentSettings(malformed) = %+v, want defaults %+v", s, want)
	}
}

func TestParseDeploymentSettingsZeroWarmReplicasAllowed(t *testing.T) {
	// 0 warm replicas is a valid (if aggressive) configuration; only negative/malformed
	// values fall back to the default.
	annotations := map[string]string{AnnotationWarmReplicas: "0"}
	s := ParseDeploymentSettings(annotations, "app-a")
	if s.WarmReplicas != 0 {
		t.Errorf("WarmReplicas = %d, want 0", s.WarmReplicas)
	}
}

func TestIsManaged(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"nil annotations", nil, false},
		{"missing annotation", map[string]string{}, false},
		{"false value", map[string]string{AnnotationManaged: "false"}, false},
		{"true value", map[string]string{AnnotationManaged: "true"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsManaged(tc.annotations); got != tc.want {
				t.Errorf("IsManaged(%v) = %v, want %v", tc.annotations, got, tc.want)
			}
		})
	}
}
