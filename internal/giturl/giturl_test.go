package giturl

import "testing"

func TestValidateGitHubRepositories(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/acme/storefront",
		"https://github.com/acme/storefront.git",
		"git@github.com:acme/storefront.git",
	} {
		if err := Validate(raw, false); err != nil {
			t.Errorf("Validate(%q) = %v", raw, err)
		}
	}
}

func TestValidateRejectsUnsafeRepositories(t *testing.T) {
	for _, raw := range []string{
		"http://github.com/acme/storefront",
		"https://github.com/acme/storefront?token=secret",
		"https://user@github.com/acme/storefront",
		"https://10.0.0.1/acme/storefront",
		"file:///etc/passwd",
		"git://redis.deephost.svc:6379/repo.git",
		"git@evil.example:acme/storefront.git",
	} {
		if err := Validate(raw, false); err == nil {
			t.Errorf("Validate(%q) accepted an unsafe repository", raw)
		}
	}
}

func TestValidateInClusterRepositoryRequiresOptIn(t *testing.T) {
	raw := "git://gitd.deephost-e2e.svc:9418/gitnext.git"
	if err := Validate(raw, false); err == nil {
		t.Fatal("in-cluster repository accepted without opt-in")
	}
	if err := Validate(raw, true); err != nil {
		t.Fatalf("in-cluster repository rejected with opt-in: %v", err)
	}
}

func TestValidateSubpath(t *testing.T) {
	for _, raw := range []string{"apps/demo", "packages/web"} {
		if err := ValidateSubpath(raw); err != nil {
			t.Errorf("ValidateSubpath(%q) = %v", raw, err)
		}
	}
	for _, raw := range []string{"/etc", "../outside", "apps/../..", `apps\\web`} {
		if err := ValidateSubpath(raw); err == nil {
			t.Errorf("ValidateSubpath(%q) accepted an unsafe path", raw)
		}
	}
}
