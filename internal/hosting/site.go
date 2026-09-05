package hosting

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/platform"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Site states, stored in SiteDoc.State and mapped onto hosting.v1.SiteState.
const (
	SiteStateAttaching = "ATTACHING"
	SiteStateReady     = "READY"
	SiteStateDegraded  = "DEGRADED"
	SiteStateDetaching = "DETACHING"
)

// Hostname roles.
const (
	HostRoleApex = "apex"
	HostRoleWWW  = "www"
	HostRoleAuto = "auto"
)

// What www.<zone> does, stored in SiteDoc.WWWMode. The empty string is the
// stored form of WWWModeServe, so a row written before the field existed keeps
// serving exactly as it did.
const (
	WWWModeServe    = "serve"
	WWWModeRedirect = "redirect"
)

// Hostname states, stored in HostnameDoc.State.
const (
	HostStateRegistered         = "REGISTERED"
	HostStateRoutesReady        = "ROUTES_READY"
	HostStateCertificatePending = "CERTIFICATE_PENDING"
	HostStateCertificateReady   = "CERTIFICATE_READY"
	HostStateMissing            = "MISSING"
	HostStateUnrouted           = "UNROUTED"
)

// maxAppIdentity is DeepHost's limit on `slug(tenant)-<app>`, which becomes a
// Kubernetes object name.
const maxAppIdentity = 63

// appDigestLength is how many hex characters of the zone-name digest go into
// the app name. Six is enough that two zones whose slugs collide (acme.dev and
// acme-dev.com both slug to "acme-dev") get different apps.
const appDigestLength = 6

// AppName is the deterministic DeepHost app name for a zone. It is a pure
// function of the tenant and the zone name, so a rebuild of the platform store
// and a detach/re-attach both land on the same app.
//
// The slug is truncated so that `slug(tenant)-<app>` fits DeepHost's 63-char
// app identity for every tenant of 1..24 characters (HOSTING_TENANT is capped
// there, spec2 §9.1).
func AppName(tenant, zoneName string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSuffix(zoneName, "."))))
	suffix := hex.EncodeToString(digest[:])[:appDigestLength]

	budget := maxAppIdentity - len(Slug(tenant)) - 1 - appDigestLength - 1
	if budget < 1 {
		budget = 1
	}
	base := Slug(zoneName)
	if len(base) > budget {
		base = base[:budget]
	}
	base = strings.Trim(base, "-")
	if base == "" {
		base = "zone"
	}
	return base + "-" + suffix
}

// AppRef is the Kubernetes App name DeepHost derives from tenant and app. The
// engine returns it from CreateApp; this function reproduces it so a resumed
// attach that skips CreateApp still knows the auto hostname.
func AppRef(tenant, app string) string { return Slug(tenant) + "-" + Slug(app) }

// Slug mirrors DeepHost's slug (cmd/controlplane/main.go): lowercase, runs of
// characters outside [a-z0-9-] collapsed to a single dash, dashes trimmed.
func Slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	out.Grow(len(value))
	dash := false
	for _, char := range value {
		switch {
		case (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9'):
			out.WriteRune(char)
			dash = false
		default:
			if !dash && out.Len() > 0 {
				out.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(out.String(), "-")
}

// hostnameState maps one engine DomainView onto the stored hostname state. A
// server that does not report readiness leaves every host REGISTERED, which is
// all the facade honestly knows.
func hostnameState(view DomainView) string {
	if !view.ReadinessReported {
		return HostStateRegistered
	}
	if !view.Ready {
		return HostStateRegistered
	}
	switch view.TLSMode {
	case TLSModeNone:
		return HostStateUnrouted
	case TLSModeShared:
		return HostStateRoutesReady
	case TLSModeIssued:
		if view.CertificateReady {
			return HostStateCertificateReady
		}
		return HostStateCertificatePending
	default:
		return HostStateRoutesReady
	}
}

// hostSettled reports whether the watcher can stop polling this host quickly.
func hostSettled(state string) bool {
	switch state {
	case HostStateCertificateReady, HostStateRoutesReady, HostStateUnrouted:
		return true
	default:
		return false
	}
}

// desiredHosts is the ordered host list a site registers with the engine.
func (s *Service) desiredHosts(doc platform.SiteDoc) []platform.HostnameDoc {
	hosts := []platform.HostnameDoc{{Host: doc.ZoneName, Role: HostRoleApex, State: HostStateMissing}}
	if doc.WWW {
		hosts = append(hosts, platform.HostnameDoc{Host: "www." + doc.ZoneName, Role: HostRoleWWW, State: HostStateMissing})
	}
	if doc.AutoHostname != "" {
		hosts = append(hosts, platform.HostnameDoc{Host: doc.AutoHostname, Role: HostRoleAuto, State: HostStateMissing})
	}
	return hosts
}

// mergeHostnames keeps the observed state of hosts that are still desired and
// drops the rest, so a site that turns www off stops reporting a www row.
func mergeHostnames(desired, observed []platform.HostnameDoc) []platform.HostnameDoc {
	byHost := make(map[string]platform.HostnameDoc, len(observed))
	for _, host := range observed {
		byHost[host.Host] = host
	}
	merged := make([]platform.HostnameDoc, 0, len(desired))
	for _, host := range desired {
		if previous, ok := byHost[host.Host]; ok {
			previous.Role = host.Role
			merged = append(merged, previous)
			continue
		}
		merged = append(merged, host)
	}
	return merged
}

func siteStateProto(state string) hostingv1.SiteState {
	switch state {
	case SiteStateAttaching:
		return hostingv1.SiteState_SITE_STATE_ATTACHING
	case SiteStateReady:
		return hostingv1.SiteState_SITE_STATE_READY
	case SiteStateDegraded:
		return hostingv1.SiteState_SITE_STATE_DEGRADED
	case SiteStateDetaching:
		return hostingv1.SiteState_SITE_STATE_DETACHING
	default:
		return hostingv1.SiteState_SITE_STATE_UNSPECIFIED
	}
}

// wwwModeProto maps the stored mode onto the wire enum. "" is SERVE.
func wwwModeProto(mode string) hostingv1.WwwMode {
	if mode == WWWModeRedirect {
		return hostingv1.WwwMode_WWW_MODE_REDIRECT
	}
	return hostingv1.WwwMode_WWW_MODE_SERVE
}

// wwwModeFromProto maps the wire enum onto the stored mode. UNSPECIFIED yields
// "", which every caller reads as "not supplied".
func wwwModeFromProto(mode hostingv1.WwwMode) string {
	switch mode {
	case hostingv1.WwwMode_WWW_MODE_SERVE:
		return WWWModeServe
	case hostingv1.WwwMode_WWW_MODE_REDIRECT:
		return WWWModeRedirect
	default:
		return ""
	}
}

// redirectTargetFor is the host the site's www host should 301 to, or "" when
// it should serve. Only the www host ever redirects: the apex is the target and
// the auto hostname is the platform's own.
func redirectTargetFor(doc platform.SiteDoc, role string) string {
	if role != HostRoleWWW || doc.WWWMode != WWWModeRedirect {
		return ""
	}
	return doc.ZoneName
}

func hostRoleProto(role string) hostingv1.HostnameRole {
	switch role {
	case HostRoleApex:
		return hostingv1.HostnameRole_HOSTNAME_ROLE_APEX
	case HostRoleWWW:
		return hostingv1.HostnameRole_HOSTNAME_ROLE_WWW
	case HostRoleAuto:
		return hostingv1.HostnameRole_HOSTNAME_ROLE_AUTO
	default:
		return hostingv1.HostnameRole_HOSTNAME_ROLE_UNSPECIFIED
	}
}

func hostStateProto(state string) hostingv1.HostnameState {
	switch state {
	case HostStateRegistered:
		return hostingv1.HostnameState_HOSTNAME_STATE_REGISTERED
	case HostStateRoutesReady:
		return hostingv1.HostnameState_HOSTNAME_STATE_ROUTES_READY
	case HostStateCertificatePending:
		return hostingv1.HostnameState_HOSTNAME_STATE_CERTIFICATE_PENDING
	case HostStateCertificateReady:
		return hostingv1.HostnameState_HOSTNAME_STATE_CERTIFICATE_READY
	case HostStateMissing:
		return hostingv1.HostnameState_HOSTNAME_STATE_MISSING
	case HostStateUnrouted:
		return hostingv1.HostnameState_HOSTNAME_STATE_UNROUTED
	default:
		return hostingv1.HostnameState_HOSTNAME_STATE_UNSPECIFIED
	}
}

// FrameworkProto maps a stored framework onto the wire enum.
func FrameworkProto(framework string) hostingv1.Framework {
	switch Framework(framework) {
	case FrameworkStatic:
		return hostingv1.Framework_FRAMEWORK_STATIC
	case FrameworkNode:
		return hostingv1.Framework_FRAMEWORK_NODE
	case FrameworkNextJS:
		return hostingv1.Framework_FRAMEWORK_NEXTJS
	default:
		return hostingv1.Framework_FRAMEWORK_UNSPECIFIED
	}
}

// FrameworkFromProto maps the wire enum onto a stored framework. The zero enum
// value yields "", which the caller treats as "not supplied".
func FrameworkFromProto(framework hostingv1.Framework) Framework {
	switch framework {
	case hostingv1.Framework_FRAMEWORK_STATIC:
		return FrameworkStatic
	case hostingv1.Framework_FRAMEWORK_NODE:
		return FrameworkNode
	case hostingv1.Framework_FRAMEWORK_NEXTJS:
		return FrameworkNextJS
	default:
		return ""
	}
}

// siteProto renders one stored site. live and latest are looked up by the
// caller, which has the store handle.
func siteProto(doc platform.SiteDoc, live, latest *hostingv1.Deploy) *hostingv1.Site {
	site := &hostingv1.Site{
		ZoneId:         doc.ZoneID,
		ZoneName:       doc.ZoneName,
		App:            doc.App,
		AppRef:         doc.AppRef,
		Framework:      FrameworkProto(doc.Framework),
		Repository:     doc.Repository,
		Branch:         doc.Branch,
		Path:           doc.Path,
		State:          siteStateProto(doc.State),
		Reason:         doc.Reason,
		Www:            doc.WWW,
		AutoHostname:   doc.AutoHostname,
		LiveDeployId:   doc.LiveDeployID,
		LatestDeployId: doc.LatestDeployID,
		Live:           live,
		Latest:         latest,
		ZoneMissing:    doc.ZoneMissing,
		WwwMode:        wwwModeProto(doc.WWWMode),
		Dns: &hostingv1.SiteDns{
			InSync:        doc.DNSInSync,
			ApexAddresses: append([]string(nil), doc.ApexAddresses...),
			RecordIds:     append([]string(nil), doc.RecordIDs...),
			Problems:      append([]string(nil), doc.DNSProblems...),
		},
	}
	if doc.WWW {
		site.Dns.WwwTarget = doc.ZoneName + "."
	}
	if !doc.ReconciledAt.IsZero() {
		site.Dns.ReconciledAt = timestamppb.New(doc.ReconciledAt.UTC())
	}
	if !doc.CreatedAt.IsZero() {
		site.CreatedAt = timestamppb.New(doc.CreatedAt.UTC())
	}
	if !doc.UpdatedAt.IsZero() {
		site.UpdatedAt = timestamppb.New(doc.UpdatedAt.UTC())
	}
	for _, host := range doc.Hostnames {
		entry := &hostingv1.Hostname{
			Host:       host.Host,
			Role:       hostRoleProto(host.Role),
			State:      hostStateProto(host.State),
			TlsMode:    host.TLSMode,
			Reason:     host.Reason,
			Retrying:   host.Retrying,
			RedirectTo: host.RedirectTo,
		}
		if !host.ObservedAt.IsZero() {
			entry.ObservedAt = timestamppb.New(host.ObservedAt.UTC())
		}
		site.Hostnames = append(site.Hostnames, entry)
	}
	return site
}

// touch stamps UpdatedAt on a site document.
func touch(doc *platform.SiteDoc, now time.Time) {
	doc.V = platform.DocVersion
	doc.UpdatedAt = now.UTC()
}
