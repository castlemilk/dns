package hosting

import "os"

// crashAfterCreateGitBuildEnv is the test-only hook spec2 §10.3 step 11 drives:
// it simulates the one crash window the build-id recovery path exists for — the
// engine has created the build, but the control plane died before the id reached
// the store — so an operator can watch the deploy come back as
// "build id recovered from the engine" instead of FAILED (§12.8).
//
// It is deliberately crude: the point is to lose the process mid-write, not to
// unwind it. It is inert unless the control plane is running with
// DNS_PRODUCTION=false AND the variable is exactly "1", so it cannot fire on a
// production control plane even if the variable leaks into its environment.
const crashAfterCreateGitBuildEnv = "DEEPHOST_TEST_CRASH_AFTER_CREATE_GIT_BUILD"

// crashAfterCreateGitBuild exits the process when the hook above is armed. It
// returns normally in every other case, which is every real deployment.
func (s *Service) crashAfterCreateGitBuild() {
	if s.cfg.Production || os.Getenv(crashAfterCreateGitBuildEnv) != "1" {
		return
	}
	s.log().Error("test hook: exiting after CreateGitBuild, before the build id is persisted",
		"hook", crashAfterCreateGitBuildEnv)
	os.Exit(1)
}
