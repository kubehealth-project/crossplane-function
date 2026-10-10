# function-kubehealth

[![CI](https://github.com/kubehealth-project/function-kubehealth/actions/workflows/ci.yml/badge.svg)](https://github.com/kubehealth-project/function-kubehealth/actions/workflows/ci.yml)

`function-kubehealth` is a Crossplane Composition Function that assesses
composed resources with the shared [KubeHealth][kubehealth] library and
publishes an aggregate report on the composite resource at
`status.kubeHealth`.

KubeHealth represents resource health as three independent dimensions:

- Reconciliation - whether the resource reached its latest desired state
- Availability - whether the resource currently provides its capability
- Lifecycle - whether the resource is initializing, active, or terminating

A failed rollout can therefore remain available while its previous revision is
still serving:

```yaml
status:
  kubeHealth:
    contractVersion: v1alpha1
    observedGeneration: 2
    reconciliation:
      status: Failed
    availability:
      status: Available
    lifecycle:
      status: Active
```

## Install

Once a release is available, install the Function package from GHCR:

```yaml
apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: function-kubehealth
spec:
  package: ghcr.io/kubehealth-project/function-kubehealth:v0.1.0
```

The initial implementation has not been released yet. Until then, use a local
package build as described under [Development](#development).

## Use

Place `function-kubehealth` after all resource-producing functions and after
any other function that writes composite status:

```yaml
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: example
spec:
  compositeTypeRef:
    apiVersion: example.org/v1alpha1
    kind: Application
  mode: Pipeline
  pipeline:
  - step: create-resources
    functionRef:
      name: function-go-templating
    input:
      apiVersion: gotemplating.fn.crossplane.io/v1beta1
      kind: GoTemplate
      source: Inline
      inline:
        template: |
          apiVersion: apps/v1
          kind: Deployment
          metadata:
            annotations:
              gotemplating.fn.crossplane.io/composition-resource-name: deployment
          spec:
            replicas: 1
            selector:
              matchLabels:
                app: example
            template:
              metadata:
                labels:
                  app: example
              spec:
                containers:
                - name: example
                  image: nginx:alpine
  - step: propagate-kubehealth
    functionRef:
      name: function-kubehealth
```

The XRD must declare `status.kubeHealth`. A complete schema is included in
[`example/xrd.yaml`](example/xrd.yaml).

The function preserves desired resources, readiness decisions, pipeline
context, and composite status fields produced by earlier steps. An earlier step
must not write `status.kubeHealth` because this function expects to own the
complete field.

## Behavior

- The final desired resource map defines the expected child set.
- A desired resource that is not observed yet is `InProgress + Unknown`.
- A composite with pending desired resources reports lifecycle `Initializing`.
- A desired and observed resource with different GVKs is treated as pending.
- Observed resources absent from final desired state are ignored.
- Child reconciliation and availability use KubeHealth's standard aggregation.
- Composite lifecycle is derived from its own deletion timestamp.
- Per-dimension transition times are retained while status values remain the
  same.
- An XR generation advancement forces reconciliation to `InProgress`, unless a
  child is already definitively failed. Current availability is retained.
- KubeHealth does not alter Crossplane's readiness signal.

## Limitations

- The function sees only objects included in Crossplane's function request. It
  does not query the Kubernetes API for EndpointSlices or related resources.
- KubeHealth checks are registered for exact GVKs. Unknown composed CRDs retain
  generic reconciliation evidence but report unknown availability.
- Crossplane currently stops Composition Function processing before invoking
  the pipeline during XR deletion. The stored lifecycle can remain `Active`.
  Consumers should prioritize `metadata.deletionTimestamp` over a stale report.
- Composition revision changes can alter desired resources without advancing
  the XR generation. The function deliberately avoids unreliable whole-object
  drift comparison.

## Development

Run tests and static analysis:

```console
go test ./...
go vet ./...
go test -race ./...
```

Run the local render scenarios:

```console
make test-render
```

Build the non-root runtime image and Crossplane package:

```console
make package
```

The package is written to `build/function-kubehealth.xpkg`. The CI workflow
builds amd64 and arm64 packages. Pushes to `main` publish a development package
to GHCR using a pseudo-version. A manually supplied workflow version can be
used for a release package.

[kubehealth]: https://github.com/kubehealth-project/kubehealth
