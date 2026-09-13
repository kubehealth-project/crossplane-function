package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kubehealth-project/kubehealth"
	"github.com/kubehealth-project/kubehealth/catalog"
	"google.golang.org/protobuf/types/known/structpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
)

var (
	testNow      = metav1.NewTime(time.Date(2026, time.September, 13, 17, 0, 0, 0, time.UTC))
	previousTime = metav1.NewTime(time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC))
)

func TestRunFunctionDeploymentHealth(t *testing.T) {
	tests := map[string]struct {
		observed  *unstructured.Unstructured
		wantRec   kubehealth.ReconciliationStatus
		wantAvail kubehealth.AvailabilityStatus
	}{
		"desired resource not observed": {
			wantRec:   kubehealth.ReconciliationInProgress,
			wantAvail: kubehealth.AvailabilityUnknown,
		},
		"failed initial rollout": {
			observed:  deployment("broken", 1, 1, 1, 0, true),
			wantRec:   kubehealth.ReconciliationFailed,
			wantAvail: kubehealth.AvailabilityUnavailable,
		},
		"healthy rollout": {
			observed:  deployment("healthy", 1, 1, 1, 1, false),
			wantRec:   kubehealth.ReconciliationReconciled,
			wantAvail: kubehealth.AvailabilityAvailable,
		},
		"failed update remains available": {
			observed:  deployment("broken-update", 2, 2, 1, 1, true),
			wantRec:   kubehealth.ReconciliationFailed,
			wantAvail: kubehealth.AvailabilityAvailable,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := baseRequest(map[string]desiredSpec{
				"deployment": {object: deployment("desired", 1, 1, 0, 0, false)},
			})
			if tc.observed != nil {
				req.Observed.Resources = map[string]*fnv1.Resource{
					"deployment": protoResource(t, tc.observed, fnv1.Ready_READY_UNSPECIFIED),
				}
			}

			rsp := run(t, standardFunction(t), req)
			health := responseHealth(t, rsp)
			if got := healthDimension(t, health, "reconciliation", "status"); got != string(tc.wantRec) {
				t.Fatalf("reconciliation = %q, want %q", got, tc.wantRec)
			}
			if got := healthDimension(t, health, "availability", "status"); got != string(tc.wantAvail) {
				t.Fatalf("availability = %q, want %q", got, tc.wantAvail)
			}
			if got := healthDimension(t, health, "lifecycle", "status"); got != string(kubehealth.LifecycleActive) {
				t.Fatalf("lifecycle = %q, want %q", got, kubehealth.LifecycleActive)
			}
		})
	}
}

func TestRunFunctionAggregationAndChildSelection(t *testing.T) {
	t.Run("mixed children use standard aggregation", func(t *testing.T) {
		req := baseRequest(map[string]desiredSpec{
			"available":   {object: deployment("available", 1, 1, 1, 1, false)},
			"unavailable": {object: deployment("unavailable", 1, 1, 0, 0, false)},
		})
		req.Observed.Resources = map[string]*fnv1.Resource{
			"available":   protoResource(t, deployment("available", 1, 1, 1, 1, false), fnv1.Ready_READY_UNSPECIFIED),
			"unavailable": protoResource(t, deployment("unavailable", 1, 1, 0, 0, false), fnv1.Ready_READY_UNSPECIFIED),
		}

		health := responseHealth(t, run(t, standardFunction(t), req))
		if got := healthDimension(t, health, "reconciliation", "status"); got != "InProgress" {
			t.Fatalf("reconciliation = %q, want InProgress", got)
		}
		if got := healthDimension(t, health, "availability", "status"); got != "PartiallyAvailable" {
			t.Fatalf("availability = %q, want PartiallyAvailable", got)
		}
	})

	t.Run("observed resource absent from desired is ignored", func(t *testing.T) {
		req := baseRequest(map[string]desiredSpec{
			"healthy": {object: deployment("healthy", 1, 1, 1, 1, false)},
		})
		req.Observed.Resources = map[string]*fnv1.Resource{
			"healthy": protoResource(t, deployment("healthy", 1, 1, 1, 1, false), fnv1.Ready_READY_UNSPECIFIED),
			"stale":   protoResource(t, deployment("stale", 1, 1, 1, 0, true), fnv1.Ready_READY_UNSPECIFIED),
		}

		health := responseHealth(t, run(t, standardFunction(t), req))
		if got := healthDimension(t, health, "reconciliation", "status"); got != "Reconciled" {
			t.Fatalf("reconciliation = %q, want Reconciled", got)
		}
	})

	t.Run("GVK mismatch is pending", func(t *testing.T) {
		req := baseRequest(map[string]desiredSpec{
			"deployment": {object: deployment("desired", 1, 1, 1, 1, false)},
		})
		observed := deployment("observed", 1, 1, 1, 1, false)
		observed.SetAPIVersion("extensions/v1beta1")
		req.Observed.Resources = map[string]*fnv1.Resource{
			"deployment": protoResource(t, observed, fnv1.Ready_READY_UNSPECIFIED),
		}

		health := responseHealth(t, run(t, standardFunction(t), req))
		if got := healthDimension(t, health, "reconciliation", "status"); got != "InProgress" {
			t.Fatalf("reconciliation = %q, want InProgress", got)
		}
		if got := healthDimension(t, health, "reconciliation", "reason"); got != kubeHealthChildrenPendingReason {
			t.Fatalf("reason = %q, want %q", got, kubeHealthChildrenPendingReason)
		}
	})

	t.Run("no desired children is not applicable", func(t *testing.T) {
		req := baseRequest(nil)
		health := responseHealth(t, run(t, standardFunction(t), req))
		if got := healthDimension(t, health, "reconciliation", "status"); got != "NotApplicable" {
			t.Fatalf("reconciliation = %q, want NotApplicable", got)
		}
		if got := healthDimension(t, health, "availability", "status"); got != "NotApplicable" {
			t.Fatalf("availability = %q, want NotApplicable", got)
		}
	})
}

func TestRunFunctionPreservesPipelineState(t *testing.T) {
	ready := fnv1.Ready_READY_TRUE
	req := baseRequest(map[string]desiredSpec{
		"deployment": {object: deployment("desired", 1, 1, 1, 1, false), ready: ready},
	})
	req.Desired.Composite = protoResource(t, object(map[string]any{
		"apiVersion": "example.my-domain.io/v1",
		"kind":       "Website",
		"status": map[string]any{
			"replicas": float64(2),
			"address":  "10.0.0.1",
		},
	}), fnv1.Ready_READY_FALSE)
	req.Context = resource.MustStructJSON(`{"example":{"preserved":true}}`)
	beforeResource := req.Desired.Resources["deployment"].Resource.AsMap()

	rsp := run(t, standardFunction(t), req)
	status := rsp.Desired.Composite.Resource.AsMap()["status"].(map[string]any)
	if status["replicas"] != float64(2) || status["address"] != "10.0.0.1" {
		t.Fatalf("desired XR status was not preserved: %#v", status)
	}
	if rsp.Desired.Composite.Ready != fnv1.Ready_READY_FALSE {
		t.Fatalf("desired XR readiness changed to %s", rsp.Desired.Composite.Ready)
	}
	if rsp.Desired.Resources["deployment"].Ready != ready {
		t.Fatalf("composed readiness changed to %s", rsp.Desired.Resources["deployment"].Ready)
	}
	if !reflect.DeepEqual(rsp.Desired.Resources["deployment"].Resource.AsMap(), beforeResource) {
		t.Fatalf("desired composed resource changed")
	}
	if !reflect.DeepEqual(rsp.Context.AsMap(), req.Context.AsMap()) {
		t.Fatalf("pipeline context changed")
	}
	if rsp.Meta.Tag != req.Meta.Tag {
		t.Fatalf("response tag = %q, want %q", rsp.Meta.Tag, req.Meta.Tag)
	}
}

func TestRunFunctionTransitionsAndGeneration(t *testing.T) {
	t.Run("unchanged dimensions retain transition times", func(t *testing.T) {
		req := baseRequest(map[string]desiredSpec{
			"deployment": {object: deployment("desired", 1, 1, 1, 1, false)},
		})
		req.Observed.Resources = map[string]*fnv1.Resource{
			"deployment": protoResource(t, deployment("observed", 1, 1, 1, 1, false), fnv1.Ready_READY_UNSPECIFIED),
		}
		setObservedHealth(t, req, priorHealth(1, "Reconciled", "Available", "Active"))

		health := responseHealth(t, run(t, standardFunction(t), req))
		for _, dimension := range []string{"reconciliation", "availability", "lifecycle"} {
			if got := healthDimension(t, health, dimension, "lastTransitionTime"); got != previousTime.Format(time.RFC3339) {
				t.Fatalf("%s transition time = %q, want %q", dimension, got, previousTime.Format(time.RFC3339))
			}
		}
	})

	t.Run("changed dimension receives current time", func(t *testing.T) {
		req := baseRequest(map[string]desiredSpec{
			"deployment": {object: deployment("desired", 1, 1, 0, 0, false)},
		})
		req.Observed.Resources = map[string]*fnv1.Resource{
			"deployment": protoResource(t, deployment("observed", 1, 1, 0, 0, false), fnv1.Ready_READY_UNSPECIFIED),
		}
		setObservedHealth(t, req, priorHealth(1, "Reconciled", "Unavailable", "Active"))

		health := responseHealth(t, run(t, standardFunction(t), req))
		if got := healthDimension(t, health, "reconciliation", "lastTransitionTime"); got != testNow.Format(time.RFC3339) {
			t.Fatalf("reconciliation transition time = %q, want %q", got, testNow.Format(time.RFC3339))
		}
		if got := healthDimension(t, health, "availability", "lastTransitionTime"); got != previousTime.Format(time.RFC3339) {
			t.Fatalf("availability transition time = %q, want %q", got, previousTime.Format(time.RFC3339))
		}
	})

	t.Run("malformed prior report is ignored", func(t *testing.T) {
		req := baseRequest(nil)
		setObservedHealth(t, req, map[string]any{"contractVersion": "broken"})
		health := responseHealth(t, run(t, standardFunction(t), req))
		if got := healthDimension(t, health, "reconciliation", "lastTransitionTime"); got != testNow.Format(time.RFC3339) {
			t.Fatalf("transition time = %q, want %q", got, testNow.Format(time.RFC3339))
		}
	})

	t.Run("new generation is in progress and retains availability", func(t *testing.T) {
		req := baseRequest(map[string]desiredSpec{
			"deployment": {object: deployment("desired", 2, 1, 1, 1, false)},
		})
		setGeneration(t, req.Observed.Composite.Resource, 2)
		req.Observed.Resources = map[string]*fnv1.Resource{
			"deployment": protoResource(t, deployment("observed", 1, 1, 1, 1, false), fnv1.Ready_READY_UNSPECIFIED),
		}
		setObservedHealth(t, req, priorHealth(1, "Reconciled", "Available", "Active"))

		health := responseHealth(t, run(t, standardFunction(t), req))
		if got := healthDimension(t, health, "reconciliation", "status"); got != "InProgress" {
			t.Fatalf("reconciliation = %q, want InProgress", got)
		}
		if got := healthDimension(t, health, "availability", "status"); got != "Available" {
			t.Fatalf("availability = %q, want Available", got)
		}
		if got := healthDimension(t, health, "availability", "reason"); got != kubeHealthChildrenReason {
			t.Fatalf("availability reason = %q, want %q", got, kubeHealthChildrenReason)
		}
		if health["observedGeneration"] != float64(2) {
			t.Fatalf("observedGeneration = %#v, want 2", health["observedGeneration"])
		}
	})

	t.Run("failure wins on new generation", func(t *testing.T) {
		req := baseRequest(map[string]desiredSpec{
			"deployment": {object: deployment("desired", 2, 1, 1, 1, false)},
		})
		setGeneration(t, req.Observed.Composite.Resource, 2)
		req.Observed.Resources = map[string]*fnv1.Resource{
			"deployment": protoResource(t, deployment("observed", 2, 1, 1, 1, true), fnv1.Ready_READY_UNSPECIFIED),
		}
		setObservedHealth(t, req, priorHealth(1, "Reconciled", "Available", "Active"))

		health := responseHealth(t, run(t, standardFunction(t), req))
		if got := healthDimension(t, health, "reconciliation", "status"); got != "Failed" {
			t.Fatalf("reconciliation = %q, want Failed", got)
		}
	})
}

func TestRunFunctionLifecycleAndErrors(t *testing.T) {
	t.Run("deleting XR is terminating", func(t *testing.T) {
		req := baseRequest(nil)
		observed := req.Observed.Composite.Resource.AsMap()
		metadata := observed["metadata"].(map[string]any)
		metadata["deletionTimestamp"] = "2026-09-13T16:00:00Z"
		req.Observed.Composite.Resource = mustStruct(t, observed)

		health := responseHealth(t, run(t, standardFunction(t), req))
		if got := healthDimension(t, health, "lifecycle", "status"); got != "Terminating" {
			t.Fatalf("lifecycle = %q, want Terminating", got)
		}
	})

	t.Run("assessment error produces a complete unknown report and warning", func(t *testing.T) {
		req := baseRequest(map[string]desiredSpec{
			"deployment": {object: deployment("desired", 1, 1, 1, 1, false)},
		})
		req.Observed.Resources = map[string]*fnv1.Resource{
			"deployment": protoResource(t, deployment("observed", 1, 1, 1, 1, false), fnv1.Ready_READY_UNSPECIFIED),
		}
		fn := NewFunction(logging.NewNopLogger(), errorAssessor{}, func() metav1.Time { return testNow })
		rsp := run(t, fn, req)
		health := responseHealth(t, rsp)
		if got := healthDimension(t, health, "reconciliation", "status"); got != "Unknown" {
			t.Fatalf("reconciliation = %q, want Unknown", got)
		}
		if got := healthDimension(t, health, "availability", "status"); got != "Unknown" {
			t.Fatalf("availability = %q, want Unknown", got)
		}
		if len(rsp.Results) != 1 || rsp.Results[0].Severity != fnv1.Severity_SEVERITY_WARNING {
			t.Fatalf("results = %#v, want one warning", rsp.Results)
		}
		if strings.Contains(rsp.Results[0].Message, "secret") {
			t.Fatalf("warning leaked assessor details: %q", rsp.Results[0].Message)
		}
	})

	t.Run("existing desired report is rejected", func(t *testing.T) {
		req := baseRequest(nil)
		req.Desired.Composite = protoResource(t, object(map[string]any{
			"status": map[string]any{"kubeHealth": priorHealth(1, "Reconciled", "Available", "Active")},
		}), fnv1.Ready_READY_UNSPECIFIED)

		rsp := run(t, standardFunction(t), req)
		if len(rsp.Results) != 1 || rsp.Results[0].Severity != fnv1.Severity_SEVERITY_FATAL {
			t.Fatalf("results = %#v, want one fatal", rsp.Results)
		}
		if !strings.Contains(rsp.Results[0].Message, kubeHealthOwnershipReason) {
			t.Fatalf("fatal result %q does not identify ownership conflict", rsp.Results[0].Message)
		}
	})
}

type errorAssessor struct{}

func (errorAssessor) Assess(*unstructured.Unstructured) (kubehealth.Assessment, error) {
	return kubehealth.Assessment{}, errors.New("internal secret should not be reported")
}

type desiredSpec struct {
	object *unstructured.Unstructured
	ready  fnv1.Ready
}

func standardFunction(t *testing.T) *Function {
	t.Helper()
	assessor, err := catalog.NewAssessor()
	if err != nil {
		t.Fatalf("create assessor: %v", err)
	}
	return NewFunction(logging.NewNopLogger(), assessor, func() metav1.Time { return testNow })
}

func run(t *testing.T, fn *Function, req *fnv1.RunFunctionRequest) *fnv1.RunFunctionResponse {
	t.Helper()
	rsp, err := fn.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("RunFunction returned transport error: %v", err)
	}
	return rsp
}

func baseRequest(desired map[string]desiredSpec) *fnv1.RunFunctionRequest {
	xr := object(map[string]any{
		"apiVersion": "example.my-domain.io/v1",
		"kind":       "Website",
		"metadata": map[string]any{
			"name":       "example",
			"namespace":  "default",
			"generation": int64(1),
		},
	})
	req := &fnv1.RunFunctionRequest{
		Meta: &fnv1.RequestMeta{Tag: "test-tag"},
		Observed: &fnv1.State{
			Composite: &fnv1.Resource{Resource: resource.MustStructObject(xr)},
			Resources: map[string]*fnv1.Resource{},
		},
		Desired: &fnv1.State{Resources: map[string]*fnv1.Resource{}},
		Context: resource.MustStructJSON(`{"example":{"preserved":true}}`),
	}
	for name, d := range desired {
		req.Desired.Resources[name] = &fnv1.Resource{Resource: resource.MustStructObject(d.object), Ready: d.ready}
	}
	return req
}

func deployment(name string, generation, observedGeneration, updated, available int64, failed bool) *unstructured.Unstructured {
	conditions := []any{}
	if failed {
		conditions = append(conditions, map[string]any{
			"type":   "Progressing",
			"status": "False",
			"reason": "ProgressDeadlineExceeded",
		})
	}
	return object(map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":       name,
			"generation": generation,
		},
		"spec": map[string]any{
			"replicas": int64(1),
		},
		"status": map[string]any{
			"observedGeneration": observedGeneration,
			"replicas":           updated,
			"updatedReplicas":    updated,
			"availableReplicas":  available,
			"conditions":         conditions,
		},
	})
}

func priorHealth(generation int64, reconciliation, availability, lifecycle string) map[string]any {
	transition := previousTime.Format(time.RFC3339)
	return map[string]any{
		"contractVersion":    "v1alpha1",
		"observedGeneration": generation,
		"reconciliation": map[string]any{
			"status":             reconciliation,
			"lastTransitionTime": transition,
		},
		"availability": map[string]any{
			"status":             availability,
			"lastTransitionTime": transition,
		},
		"lifecycle": map[string]any{
			"status":             lifecycle,
			"lastTransitionTime": transition,
		},
	}
}

func object(content map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: content}
	if apiVersion, ok := content["apiVersion"].(string); ok {
		if kind, ok := content["kind"].(string); ok {
			gv, _ := schema.ParseGroupVersion(apiVersion)
			u.SetGroupVersionKind(gv.WithKind(kind))
		}
	}
	return u
}

func protoResource(t *testing.T, u *unstructured.Unstructured, ready fnv1.Ready) *fnv1.Resource {
	t.Helper()
	return &fnv1.Resource{Resource: resource.MustStructObject(u), Ready: ready}
}

func mustStruct(t *testing.T, content map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(content)
	if err != nil {
		t.Fatalf("create protobuf struct: %v", err)
	}
	return s
}

func setGeneration(t *testing.T, s *structpb.Struct, generation int64) {
	t.Helper()
	content := s.AsMap()
	content["metadata"].(map[string]any)["generation"] = generation
	*s = *mustStruct(t, content)
}

func setObservedHealth(t *testing.T, req *fnv1.RunFunctionRequest, health map[string]any) {
	t.Helper()
	content := req.Observed.Composite.Resource.AsMap()
	content["status"] = map[string]any{"kubeHealth": health}
	req.Observed.Composite.Resource = mustStruct(t, content)
}

func responseHealth(t *testing.T, rsp *fnv1.RunFunctionResponse) map[string]any {
	t.Helper()
	if rsp.Desired == nil || rsp.Desired.Composite == nil || rsp.Desired.Composite.Resource == nil {
		t.Fatalf("response has no desired composite resource: %#v", rsp)
	}
	content := rsp.Desired.Composite.Resource.AsMap()
	status, ok := content["status"].(map[string]any)
	if !ok {
		t.Fatalf("desired composite has no status: %#v", content)
	}
	health, ok := status["kubeHealth"].(map[string]any)
	if !ok {
		t.Fatalf("desired composite has no status.kubeHealth: %#v", status)
	}
	return health
}

func healthDimension(t *testing.T, health map[string]any, dimension, field string) string {
	t.Helper()
	d, ok := health[dimension].(map[string]any)
	if !ok {
		t.Fatalf("health has no %s dimension: %#v", dimension, health)
	}
	value, ok := d[field].(string)
	if !ok {
		t.Fatalf("health %s has no string %s: %#v", dimension, field, d)
	}
	return value
}
