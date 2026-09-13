package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kubehealth-project/kubehealth"
	healthv1alpha1 "github.com/kubehealth-project/kubehealth/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
)

const (
	kubeHealthChildrenReason        = "ChildResourcesAggregated"
	kubeHealthNoChildrenReason      = "NoApplicableChildResources"
	kubeHealthChildrenPendingReason = "ChildResourcesPending"
	kubeHealthAssessmentErrorReason = "ChildAssessmentFailed"
	kubeHealthTerminatingReason     = "DeletionRequested"
	kubeHealthActiveReason          = "DeletionNotRequested"
	kubeHealthGenerationReason      = "LatestGenerationNotObserved"
	kubeHealthOwnershipReason       = "KubeHealthOwnershipConflict"
)

// Assessor is the KubeHealth behavior used by this function.
type Assessor interface {
	Assess(*unstructured.Unstructured) (kubehealth.Assessment, error)
}

// Function propagates composed resource health to status.kubeHealth on an XR.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log      logging.Logger
	assessor Assessor
	now      func() metav1.Time
}

// NewFunction creates a Composition Function with injectable dependencies.
func NewFunction(log logging.Logger, assessor Assessor, now func() metav1.Time) *Function {
	return &Function{log: log, assessor: assessor, now: now}
}

// RunFunction assesses all finally desired composed resources and publishes a
// complete KubeHealth report on the desired XR status.
func (f *Function) RunFunction(_ context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	rsp := response.To(req, response.DefaultTTL)
	if req == nil {
		response.Fatal(rsp, fmt.Errorf("cannot run function with a nil request"))
		return rsp, nil
	}

	observedXR, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("cannot get observed composite resource: %w", err))
		return rsp, nil
	}
	if observedXR.Resource.GroupVersionKind().Empty() {
		response.Fatal(rsp, fmt.Errorf("observed composite resource GVK must not be empty"))
		return rsp, nil
	}

	desiredXR, err := request.GetDesiredCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("cannot get desired composite resource: %w", err))
		return rsp, nil
	}
	desiredXR.Ready = readyFromProto(req.GetDesired().GetComposite().GetReady())
	if _, found, err := unstructured.NestedFieldNoCopy(desiredXR.Resource.Object, "status", "kubeHealth"); err != nil {
		response.Fatal(rsp, fmt.Errorf("cannot inspect desired status.kubeHealth: %w", err))
		return rsp, nil
	} else if found {
		response.Fatal(rsp, fmt.Errorf("%s: desired status.kubeHealth is already owned by an earlier pipeline step", kubeHealthOwnershipReason))
		return rsp, nil
	}

	desired, err := request.GetDesiredComposedResources(req)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("cannot get desired composed resources: %w", err))
		return rsp, nil
	}
	observed, err := request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("cannot get observed composed resources: %w", err))
		return rsp, nil
	}

	log := f.log.WithValues(
		"xr-api-version", observedXR.Resource.GetAPIVersion(),
		"xr-kind", observedXR.Resource.GetKind(),
		"xr-name", observedXR.Resource.GetName(),
		"xr-namespace", observedXR.Resource.GetNamespace(),
	)

	assessments, observedCount, pendingCount, assessmentErrs := f.assessChildren(log, desired, observed)
	aggregated := kubehealth.AggregateChildren(assessments)
	reconciliationReason, reconciliationMessage := aggregateExplanation(len(desired), observedCount, pendingCount)
	availabilityReason, availabilityMessage := reconciliationReason, reconciliationMessage
	if len(assessmentErrs) > 0 {
		aggregated.Reconciliation = kubehealth.ReconciliationUnknown
		aggregated.Availability = kubehealth.AvailabilityUnknown
		reconciliationReason = kubeHealthAssessmentErrorReason
		reconciliationMessage = fmt.Sprintf(
			"Unable to assess %d of %d desired child resource(s); those resources are reported as unknown",
			len(assessmentErrs),
			len(desired),
		)
		availabilityReason, availabilityMessage = reconciliationReason, reconciliationMessage
		response.Warning(rsp, fmt.Errorf("%s: %s", kubeHealthAssessmentErrorReason, reconciliationMessage)).
			WithReason(kubeHealthAssessmentErrorReason)
	}

	generation := compositeGeneration(observedXR.Resource)
	previous := previousKubeHealth(&observedXR.Resource.Unstructured)
	if len(assessmentErrs) == 0 && previous != nil && previous.ObservedGeneration < generation && aggregated.Reconciliation != kubehealth.ReconciliationFailed {
		aggregated.Reconciliation = kubehealth.ReconciliationInProgress
		reconciliationReason = kubeHealthGenerationReason
		reconciliationMessage = fmt.Sprintf(
			"XR generation is %d, but the previous KubeHealth report observed generation %d",
			generation,
			previous.ObservedGeneration,
		)
	}

	health := previous
	if health == nil {
		health = healthv1alpha1.NewStatus(generation)
	}
	health.ContractVersion = healthv1alpha1.ContractVersion
	health.ObservedGeneration = generation
	now := f.now()
	health.SetReconciliationAt(aggregated.Reconciliation, reconciliationReason, reconciliationMessage, now)
	health.SetAvailabilityAt(aggregated.Availability, availabilityReason, availabilityMessage, now)
	if observedXR.Resource.GetDeletionTimestamp() != nil {
		health.SetLifecycleAt(
			kubehealth.LifecycleTerminating,
			kubeHealthTerminatingReason,
			"The composite resource is terminating",
			now,
		)
	} else {
		health.SetLifecycleAt(
			kubehealth.LifecycleActive,
			kubeHealthActiveReason,
			"The composite resource is active",
			now,
		)
	}

	value, err := runtime.DefaultUnstructuredConverter.ToUnstructured(health)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("cannot encode desired status.kubeHealth: %w", err))
		return rsp, nil
	}
	if err := unstructured.SetNestedField(desiredXR.Resource.Object, value, "status", "kubeHealth"); err != nil {
		response.Fatal(rsp, fmt.Errorf("cannot set desired status.kubeHealth: %w", err))
		return rsp, nil
	}
	if err := response.SetDesiredCompositeResource(rsp, desiredXR); err != nil {
		response.Fatal(rsp, fmt.Errorf("cannot set desired composite resource: %w", err))
		return rsp, nil
	}

	log.Info(
		"Published KubeHealth status",
		"desired-children", len(desired),
		"observed-children", observedCount,
		"pending-children", pendingCount,
		"assessment-errors", len(assessmentErrs),
		"reconciliation", aggregated.Reconciliation,
		"availability", aggregated.Availability,
		"lifecycle", health.Lifecycle.Status,
	)
	return rsp, nil
}

func (f *Function) assessChildren(
	log logging.Logger,
	desired map[resource.Name]*resource.DesiredComposed,
	observed map[resource.Name]resource.ObservedComposed,
) ([]kubehealth.Assessment, int, int, []error) {
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, string(name))
	}
	sort.Strings(names)

	assessments := make([]kubehealth.Assessment, 0, len(names))
	assessmentErrs := make([]error, 0)
	observedCount := 0
	pendingCount := 0
	for _, rawName := range names {
		name := resource.Name(rawName)
		desiredResource := desired[name]
		observedResource, found := observed[name]
		if desiredResource == nil || desiredResource.Resource == nil || !found || observedResource.Resource == nil ||
			desiredResource.Resource.GroupVersionKind() != observedResource.Resource.GroupVersionKind() {
			pendingCount++
			assessments = append(assessments, pendingAssessment(rawName))
			continue
		}

		assessment, err := f.assessor.Assess(&observedResource.Resource.Unstructured)
		if err != nil {
			wrapped := fmt.Errorf("assess desired child %q with GVK %s: %w", rawName, observedResource.Resource.GroupVersionKind(), err)
			assessmentErrs = append(assessmentErrs, wrapped)
			log.Info(
				"Unable to assess child resource",
				"logical-name", rawName,
				"gvk", observedResource.Resource.GroupVersionKind().String(),
				"error-type", fmt.Sprintf("%T", err),
			)
			assessment = unknownAssessment()
		}
		assessments = append(assessments, assessment)
		observedCount++
	}
	return assessments, observedCount, pendingCount, assessmentErrs
}

func pendingAssessment(name string) kubehealth.Assessment {
	return kubehealth.Assessment{
		Reconciliation: kubehealth.Dimension[kubehealth.ReconciliationStatus]{
			Status:  kubehealth.ReconciliationInProgress,
			Reason:  "DesiredResourceNotObserved",
			Message: fmt.Sprintf("Desired composed resource %q has not been observed with its expected GVK", name),
		},
		Availability: kubehealth.Dimension[kubehealth.AvailabilityStatus]{
			Status: kubehealth.AvailabilityUnknown,
		},
		Lifecycle: kubehealth.Dimension[kubehealth.LifecycleStatus]{
			Status: kubehealth.LifecycleActive,
		},
	}
}

func unknownAssessment() kubehealth.Assessment {
	return kubehealth.Assessment{
		Reconciliation: kubehealth.Dimension[kubehealth.ReconciliationStatus]{Status: kubehealth.ReconciliationUnknown},
		Availability:   kubehealth.Dimension[kubehealth.AvailabilityStatus]{Status: kubehealth.AvailabilityUnknown},
		Lifecycle:      kubehealth.Dimension[kubehealth.LifecycleStatus]{Status: kubehealth.LifecycleActive},
	}
}

func aggregateExplanation(desired, observed, pending int) (string, string) {
	if desired == 0 {
		return kubeHealthNoChildrenReason, "The composition has no desired composed resources to assess"
	}
	if pending > 0 {
		return kubeHealthChildrenPendingReason, fmt.Sprintf(
			"Aggregated health from %d observed child resource(s); %d desired resource(s) are pending",
			observed,
			pending,
		)
	}
	return kubeHealthChildrenReason, fmt.Sprintf("Aggregated health from %d observed child resource(s)", observed)
}

func previousKubeHealth(xr *unstructured.Unstructured) *healthv1alpha1.Status {
	value, found, err := unstructured.NestedMap(xr.Object, "status", "kubeHealth")
	if err != nil || !found {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var health healthv1alpha1.Status
	if err := json.Unmarshal(encoded, &health); err != nil {
		return nil
	}
	if err := health.Validate(); err != nil {
		return nil
	}
	return &health
}

func compositeGeneration(xr interface {
	GetGeneration() int64
	GetInteger(string) (int64, error)
},
) int64 {
	if generation, err := xr.GetInteger("metadata.generation"); err == nil {
		return generation
	}
	return xr.GetGeneration()
}

func readyFromProto(ready fnv1.Ready) resource.Ready {
	switch ready {
	case fnv1.Ready_READY_TRUE:
		return resource.ReadyTrue
	case fnv1.Ready_READY_FALSE:
		return resource.ReadyFalse
	default:
		return resource.ReadyUnspecified
	}
}
