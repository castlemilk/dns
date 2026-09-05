# Which DeepHost changes are ours vs the owner's own uncommitted work

The DeepHost working tree at ~/projects/deephost has 66 modified + 4 untracked files against HEAD
(3ab3e16). It mixes the owner's own uncommitted batch (present before this session started) with the
additive changes made for the "simple" console. The baseline that separated them was lost when the
session scratchpad was wiped; this is the reconstruction, verified from the diffs.

## The proto is the clearest signal — 8 added RPCs
Owner's (already present at session start, 2026-09-02 21:44):
  UpdateApp, GetBuildLogs, GetMetrics
Ours (phase 2, "WP8"):
  ListBuilds, DeleteDomain
Ours (phase 3, gap closing):
  SetAppEnvVar, DeleteAppEnvVar, ListAppEnvVars
Plus our added message fields: BuildView.{created,started,finished}_unix_ms/revision/repo_url/environment,
ReleaseView.created_unix_ms, DomainView.{readiness_reported,ready,tls_mode,certificate_ready,reason,redirect_to},
CreateDomainRequest.redirect_to.

## Files containing our work (everything else in the diff is the owner's)
  proto/deephost/v1/controlplane.proto              both (see split above)
  gen/go/deephost/v1/controlplane.pb.go             both (regenerated)
  gen/go/deephost/v1/deephostv1connect/controlplane.connect.go  both (regenerated)
  api/v1alpha1/build_types.go                       ours: BuildStatus.StartedAt/FinishedAt
  api/v1alpha1/domain_types.go                      ours: DomainSpec.RedirectTo
  api/v1alpha1/app_types.go                         ours: AnnotationEnvLastSet, EnvSecretName, EnvSecretDataKey
  api/v1alpha1/zz_generated.deepcopy.go             ours: deepcopy for the above
  cmd/controlplane/main.go                          both — ours: ListBuilds, DeleteDomain, the three env RPCs,
                                                    DomainView projection, redirect_to validation
  cmd/controlplane/main_test.go                     both — ours: the matching tests
  cmd/operator/build_controller.go                  ours: timestamp stamping, manifestGitSHA, Status().Update fix
  cmd/operator/build_controller_test.go             ours
  cmd/operator/main.go                              ours: artifactStore wiring for the manifest read
  cmd/builder/main.go                               ours: gitHeadSHA (git rev-parse HEAD)
  cmd/builder/main_test.go                          ours
  internal/gateway/provider.go                      ours: redirect filter + certificate read for readiness
  internal/gateway/provider_test.go                 ours
  charts/deephost/templates/rbac.yaml               ours: configmaps get, cert-manager certificates get/list
  charts/deephost/crds/builds.deephost.io.yaml      ours: status.startedAt/finishedAt
  charts/deephost/crds/domains.deephost.io.yaml     ours: spec.redirectTo
  docs/mcp.md, docs/product.md                      ours: notes on the new RPCs

## Rollout
`kubectl apply -f charts/deephost/crds` must run BEFORE the new operator image ships, or the API server
prunes status.startedAt/finishedAt/gitSha with no error and the timestamps silently vanish.

## Two latent bugs we fixed (worth keeping regardless of the console)
1. cmd/builder wrote the *requested* revision into the artifact manifest's gitSha, never the resolved
   commit, so Release.status.gitSha could only ever have held a branch name.
2. ensureRelease passed a Status literal to Create; Release has a status subresource, so the API server
   discarded it every time. Fixed with an explicit Status().Update.
