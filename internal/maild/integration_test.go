package maild_test

// Adversarial verification of one question only: does the REAL frozen client in
// internal/mail/stalwart drive the new server over a REAL socket?
//
// Nothing here uses an in-process RoundTripper, a fake engine or the server's
// own opinion of itself. A maild.Server is started on ephemeral loopback ports,
// a genuine *stalwart.Client is pointed at the address it actually bound, and
// every Engine method the facade calls is exercised against it. The wiring is
// copied verbatim from cmd/deephost-mail/services.go, because that file is
// package main and cannot be imported.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/mail"
	"github.com/castlemilk/dns/internal/mail/stalwart"
	"github.com/castlemilk/dns/internal/maild"
)

const (
	testHostname = "mail.integration.test"
	testToken    = "0123456789abcdef0123456789abcdef0123456789ab"
	testZone     = "customer-zone.test"
)

// liveServer starts a real maild.Server on ephemeral ports and returns it with
// a real stalwart.Client aimed at the bound management address.
func liveServer(t *testing.T) (*maild.Server, *stalwart.Client) {
	t.Helper()

	server, err := maild.NewServer(maild.Config{
		Hostname:   testHostname,
		AdminToken: maild.Secret(testToken),
		DataDir:    t.TempDir(),
		APIAddr:    "127.0.0.1:0",
		SMTPAddr:   "127.0.0.1:0",
		POP3Addr:   "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Services:   productionServices(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		drop(server.Shutdown(shutdownCtx))
	})

	addr := server.Addr(maild.ListenerAPI)
	if addr == "" {
		t.Fatal("the management listener reported no address")
	}
	client, err := stalwart.New("http://"+addr, testToken)
	if err != nil {
		t.Fatalf("stalwart.New: %v", err)
	}
	// The facade only ever holds the client behind this interface; if the
	// server cannot be driven through it, nothing else matters.
	var _ mail.Engine = client
	return server, client
}

// productionServices mirrors cmd/deephost-mail/services.go exactly.
func productionServices() func(maild.Deps) (maild.Services, error) {
	return func(deps maild.Deps) (maild.Services, error) {
		deliverer, err := maild.NewDeliverer(deps.Store, maild.DeliveryConfig{
			Root:     deps.DataDir,
			Hostname: deps.Hostname,
			Logger:   deps.Logger,
			Now:      deps.Clock,
		})
		if err != nil {
			return maild.Services{}, err
		}
		smtp, err := maild.NewSMTPServer(deliverer, maild.SMTPConfig{
			Hostname:  deps.Hostname,
			TLSConfig: deps.TLS,
			Logger:    deps.Logger,
			Now:       deps.Clock,
		})
		if err != nil {
			return maild.Services{}, err
		}
		pop3 := maild.NewPOP3Server(deps.Store, maild.NewMaildir(deps.DataDir),
			maild.POP3WithHostname(deps.Hostname),
			maild.POP3WithLogger(deps.Logger),
		)
		api, err := maild.NewHandler(maild.Options{
			Store:    deps.Store,
			Token:    string(deps.AdminToken.Bytes()),
			Hostname: deps.Hostname,
			Logger:   deps.Logger,
			Now:      deps.Clock,
			RenderZoneFile: func(ctx context.Context, domain maild.Domain) (string, error) {
				keys, err := deps.Store.ListDkimKeys(ctx, domain.ID)
				if err != nil {
					return "", err
				}
				return maild.DkimZoneRecords(keys, domain.Name), nil
			},
			ProvisionDkim: func(ctx context.Context, domain maild.Domain, algorithms []string, selectorTemplate string) error {
				_, err := maild.ProvisionDkimKeys(ctx, deps.Store, domain.ID, maild.DkimOptions{
					SelectorTemplate: selectorTemplate,
					Algorithms:       algorithms,
					Now:              deps.Clock(),
				})
				return err
			},
			ListQueue: func(ctx context.Context) ([]maild.QueuedMessage, error) {
				return deps.Store.ListQueuedMessages(ctx, maild.MaxObjectsPerCall)
			},
		})
		if err != nil {
			return maild.Services{}, err
		}
		return maild.Services{
			API:        api,
			SMTP:       sessions(smtp),
			Submission: sessions(smtp),
			POP3:       maild.SessionHandlerFunc(pop3.ServeConn),
		}, nil
	}
}

// drop names an error a test deliberately ignores: a close or a shutdown whose
// failure cannot change what the assertions already proved. errcheck's
// check-blank rule refuses the blank identifier for this.
func drop(error) {}

func sessions(server *maild.SMTPServer) maild.SessionHandler {
	return maild.SessionHandlerFunc(func(_ context.Context, conn net.Conn) { server.ServeConn(conn) })
}

// TestRealClientDrivesTheRealServer walks the whole facade lifecycle over TCP.
func TestRealClientDrivesTheRealServer(t *testing.T) {
	server, client := liveServer(t)
	ctx := context.Background()

	// --- Account ------------------------------------------------------------
	info, err := client.Account(ctx)
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if info.Edition == "" {
		t.Error("Account reported no edition")
	}
	held := map[string]bool{}
	for _, name := range info.Permissions {
		held[name] = true
	}
	for _, required := range mail.RequiredPermissions {
		if !held[required] {
			t.Errorf("Account is missing the required permission %q", required)
		}
	}

	// --- SystemSettings -----------------------------------------------------
	settings, err := client.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if settings.DefaultHostname != testHostname {
		t.Errorf("SystemSettings.DefaultHostname = %q, want %q — the facade refuses every operation on a mismatch", settings.DefaultHostname, testHostname)
	}
	if len(settings.MailExchangers) == 0 {
		t.Error("SystemSettings reported no mail exchangers")
	}

	// --- CreateDomain -------------------------------------------------------
	domain, err := client.CreateDomain(ctx, stalwart.DomainInput{
		Name:             testZone,
		Description:      "integration",
		ReportAddressURI: "mailto:dmarc@" + testZone,
	})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if domain.ID == "" || domain.Name != testZone || !domain.Enabled {
		t.Fatalf("CreateDomain returned %+v", domain)
	}
	if domain.Description != "integration" {
		t.Errorf("CreateDomain description = %q, want %q", domain.Description, "integration")
	}
	if domain.CreatedAt.IsZero() {
		t.Error("CreateDomain returned a zero createdAt")
	}
	if !strings.Contains(domain.DNSZoneFile, "_domainkey") {
		t.Errorf("CreateDomain returned no DKIM records in dnsZoneFile: %q", domain.DNSZoneFile)
	}

	// --- FindDomain ---------------------------------------------------------
	found, ok, err := client.FindDomain(ctx, testZone)
	if err != nil || !ok {
		t.Fatalf("FindDomain(%q) = %v, %v, %v", testZone, found, ok, err)
	}
	if found.ID != domain.ID {
		t.Errorf("FindDomain returned id %q, want %q", found.ID, domain.ID)
	}
	if _, ok, err := client.FindDomain(ctx, strings.ToUpper(testZone)); err != nil || !ok {
		t.Errorf("FindDomain is not case-insensitive: ok=%v err=%v", ok, err)
	}
	if _, ok, err := client.FindDomain(ctx, "absent.test"); err != nil || ok {
		t.Errorf("FindDomain(absent) = %v, %v; want false, nil", ok, err)
	}

	// --- ListDomains --------------------------------------------------------
	domains, err := client.ListDomains(ctx)
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if len(domains) != 1 || domains[0].ID != domain.ID {
		t.Fatalf("ListDomains = %+v, want exactly the created domain", domains)
	}

	// --- GetDomain ----------------------------------------------------------
	got, err := client.GetDomain(ctx, domain.ID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	if got.DNSZoneFile == "" {
		t.Error("GetDomain returned an empty dnsZoneFile; the facade would bind without DKIM")
	}

	// --- ListDkim -----------------------------------------------------------
	signatures, err := client.ListDkim(ctx, domain.ID)
	if err != nil {
		t.Fatalf("ListDkim: %v", err)
	}
	algorithms := map[string]bool{}
	for _, signature := range signatures {
		if !signature.Active() {
			t.Errorf("DKIM signature %q is %q, not active; the facade's 20s bind budget wants one active per algorithm", signature.Selector, signature.Stage)
		}
		if signature.PublicKey == "" {
			t.Errorf("DKIM signature %q has no public key", signature.Selector)
		}
		algorithms[signature.Algorithm] = true
	}
	for _, wanted := range stalwart.RequestedDkimAlgorithms {
		if !algorithms[wanted] {
			t.Errorf("ListDkim reported no %s signature; got %+v", wanted, signatures)
		}
	}

	// --- CreateAccount ------------------------------------------------------
	account, err := client.CreateAccount(ctx, stalwart.AccountInput{
		LocalPart:   "ada",
		DomainID:    domain.ID,
		Description: "Ada Lovelace",
		Secret:      "correct-horse-battery-staple",
		QuotaBytes:  1 << 30,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if account.EmailAddress != "ada@"+testZone {
		t.Errorf("CreateAccount emailAddress = %q", account.EmailAddress)
	}
	if account.QuotaBytes != 1<<30 {
		t.Errorf("CreateAccount quota = %d, want %d", account.QuotaBytes, uint64(1)<<30)
	}
	if account.Description != "Ada Lovelace" {
		t.Errorf("CreateAccount description = %q", account.Description)
	}

	// --- ListAccounts -------------------------------------------------------
	accounts, err := client.ListAccounts(ctx, domain.ID)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != account.ID {
		t.Fatalf("ListAccounts = %+v, want exactly the created account", accounts)
	}

	// --- UpdateAccount ------------------------------------------------------
	newQuota := uint64(2) << 30
	aliases := []stalwart.Alias{{Name: "info", DomainID: domain.ID, Enabled: true}}
	secret := "a-new-passphrase-entirely"
	if err := client.UpdateAccount(ctx, account.ID, stalwart.AccountPatch{
		QuotaBytes: &newQuota,
		Aliases:    &aliases,
		Secret:     &secret,
	}); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	updated, err := client.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("GetAccount after update: %v", err)
	}
	if updated.QuotaBytes != newQuota {
		t.Errorf("quota after update = %d, want %d", updated.QuotaBytes, newQuota)
	}
	if len(updated.Aliases) != 1 || updated.Aliases[0].Name != "info" {
		t.Errorf("aliases after update = %+v, want one named info", updated.Aliases)
	}

	// --- CreateList ---------------------------------------------------------
	list, err := client.CreateList(ctx, stalwart.ListInput{
		LocalPart:   "team",
		DomainID:    domain.ID,
		Description: stalwart.ListDescription,
		Recipients:  []string{"elsewhere@external.test"},
	})
	if err != nil {
		t.Fatalf("CreateList: %v", err)
	}
	lists, err := client.ListLists(ctx, domain.ID)
	if err != nil {
		t.Fatalf("ListLists: %v", err)
	}
	if len(lists) != 1 || lists[0].ID != list.ID {
		t.Fatalf("ListLists = %+v, want exactly the created list", lists)
	}
	if lists[0].EmailAddress != "team@"+testZone {
		t.Errorf("list emailAddress = %q", lists[0].EmailAddress)
	}

	// --- QueryQueue over a real delivery -----------------------------------
	// Inbound mail to the forwarder fans out to an external target, which the
	// delivery path queues. The queue is the only honestly knowable mail
	// figure the console shows, so it has to be attributable to the zone.
	deliver(t, server.Addr(maild.ListenerSMTP), "sender@somewhere.test", "team@"+testZone)

	messages, complete, err := client.QueryQueue(ctx, testZone, 50)
	if err != nil {
		t.Fatalf("QueryQueue: %v", err)
	}
	if !complete {
		t.Error("QueryQueue reported an incomplete page for a single message")
	}
	if len(messages) != 1 {
		t.Fatalf("QueryQueue returned %d messages, want 1 for the fan-out just delivered", len(messages))
	}
	if len(messages[0].Recipients) != 1 {
		t.Fatalf("queued message has %d recipients: %+v", len(messages[0].Recipients), messages[0])
	}
	recipient := messages[0].Recipients[0]
	if recipient.Address != "elsewhere@external.test" {
		t.Errorf("queued recipient address = %q, want the external target", recipient.Address)
	}
	if recipient.ORCPT != "team@"+testZone {
		t.Errorf("queued recipient orcpt = %q, want the customer's address", recipient.ORCPT)
	}
	if messages[0].CreatedAt.IsZero() {
		t.Error("queued message has a zero createdAt")
	}
	// The same message must NOT be attributed to somebody else's zone.
	other, _, err := client.QueryQueue(ctx, "not-the-customer.test", 50)
	if err != nil {
		t.Fatalf("QueryQueue(other zone): %v", err)
	}
	if len(other) != 0 {
		t.Errorf("QueryQueue leaked %d message(s) into an unrelated zone", len(other))
	}

	// --- DestroyAccount -----------------------------------------------------
	if err := client.DestroyAccount(ctx, account.ID); err != nil {
		t.Fatalf("DestroyAccount: %v", err)
	}
	remaining, err := client.ListAccounts(ctx, domain.ID)
	if err != nil {
		t.Fatalf("ListAccounts after destroy: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("ListAccounts still reports %d account(s) after DestroyAccount", len(remaining))
	}
	// Destroying it again is success: the caller is making its absence true.
	if err := client.DestroyAccount(ctx, account.ID); err != nil {
		t.Errorf("DestroyAccount is not idempotent: %v", err)
	}
}

// TestUnauthorizedIsReportedAsUnauthorized proves the 401 the facade reasons
// about actually comes back over the wire, and that the server is not simply
// open.
func TestUnauthorizedIsReportedAsUnauthorized(t *testing.T) {
	server, _ := liveServer(t)
	wrong, err := stalwart.New("http://"+server.Addr(maild.ListenerAPI), "not-the-admin-token-not-even-close")
	if err != nil {
		t.Fatalf("stalwart.New: %v", err)
	}
	if _, err := wrong.Account(context.Background()); !isUnauthorized(err) {
		t.Errorf("Account with a bad token = %v, want ErrUnauthorized", err)
	}
	if _, err := wrong.SystemSettings(context.Background()); !isUnauthorized(err) {
		t.Errorf("SystemSettings with a bad token = %v, want ErrUnauthorized", err)
	}
}

func isUnauthorized(err error) bool {
	return err != nil && strings.Contains(err.Error(), stalwart.ErrUnauthorized.Error())
}

// deliver speaks SMTP to the server's own port 25 listener and delivers one
// message. No fake: this is the same path a remote MTA takes.
func deliver(t *testing.T, addr, from, to string) {
	t.Helper()
	if addr == "" {
		t.Fatal("the SMTP listener reported no address")
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial smtp: %v", err)
	}
	defer func() { drop(conn.Close()) }()
	drop(conn.SetDeadline(time.Now().Add(20 * time.Second)))

	reader := textproto.NewReader(bufio.NewReader(conn))
	expect := func(want byte, context string) {
		t.Helper()
		code, line, err := reader.ReadResponse(int(want - '0'))
		if err != nil {
			t.Fatalf("smtp %s: %v (code %d, %q)", context, err, code, line)
		}
	}
	send := func(format string, args ...any) {
		t.Helper()
		if _, err := fmt.Fprintf(conn, format+"\r\n", args...); err != nil {
			t.Fatalf("smtp write: %v", err)
		}
	}

	expect('2', "banner")
	send("EHLO integration.test")
	expect('2', "EHLO")
	send("MAIL FROM:<%s>", from)
	expect('2', "MAIL FROM")
	send("RCPT TO:<%s>", to)
	expect('2', "RCPT TO")
	send("DATA")
	expect('3', "DATA")
	send("From: <%s>\r\nTo: <%s>\r\nSubject: integration\r\n\r\nhello\r\n.", from, to)
	expect('2', "end of DATA")
	send("QUIT")
}

// TestListPagingSurvivesMoreThanOnePage is the loop guard. ListLists and
// ListDomains page with `position += 500` and stop when a page comes back
// short; a server that ignored `position` would make the client loop forever
// once a deployment held 500 objects. 501 mailing lists is the cheap way to
// prove the paging is real, since a domain costs two RSA keypairs.
func TestListPagingSurvivesMoreThanOnePage(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 501 objects")
	}
	_, client := liveServer(t)
	ctx := context.Background()

	domain, err := client.CreateDomain(ctx, stalwart.DomainInput{Name: "paging.test"})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	const want = stalwart.MaxObjectsPerCall + 1
	for index := 0; index < want; index++ {
		if _, err := client.CreateList(ctx, stalwart.ListInput{
			LocalPart:  fmt.Sprintf("list-%03d", index),
			DomainID:   domain.ID,
			Recipients: []string{"target@external.test"},
		}); err != nil {
			t.Fatalf("CreateList %d: %v", index, err)
		}
	}

	type result struct {
		lists []stalwart.MailingList
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lists, err := client.ListLists(ctx, domain.ID)
		done <- result{lists, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("ListLists: %v", got.err)
		}
		if len(got.lists) != want {
			t.Errorf("ListLists returned %d lists, want %d", len(got.lists), want)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("ListLists did not terminate: the server is not honouring `position`, so the client pages forever")
	}
}

// TestMailboxCreatedByTheClientCollectsItsMail is the outcome the feature
// exists for. The facade creates a mailbox with a generated credential and
// shows that credential to a customer once; if the customer cannot then log in
// over the published implicit-TLS port and read the mail that arrived, every
// assertion above is about a control surface with nothing behind it.
func TestMailboxCreatedByTheClientCollectsItsMail(t *testing.T) {
	certFile, keyFile := selfSignedPair(t)
	dataDir := t.TempDir()
	server, err := maild.NewServer(maild.Config{
		Hostname:    testHostname,
		AdminToken:  maild.Secret(testToken),
		DataDir:     dataDir,
		APIAddr:     "127.0.0.1:0",
		SMTPAddr:    "127.0.0.1:0",
		POP3SAddr:   "127.0.0.1:0",
		TLSCertFile: certFile,
		TLSKeyFile:  keyFile,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Services:    productionServices(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		drop(server.Shutdown(shutdownCtx))
	})

	client, err := stalwart.New("http://"+server.Addr(maild.ListenerAPI), testToken)
	if err != nil {
		t.Fatalf("stalwart.New: %v", err)
	}
	domain, err := client.CreateDomain(ctx, stalwart.DomainInput{Name: testZone})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	const passphrase = "one-generated-credential-shown-once"
	account, err := client.CreateAccount(ctx, stalwart.AccountInput{
		LocalPart: "ada", DomainID: domain.ID, Secret: passphrase, QuotaBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	deliver(t, server.Addr(maild.ListenerSMTP), "sender@somewhere.test", account.EmailAddress)

	// Usage must reflect the delivery, because that number is shown per mailbox.
	stored, err := client.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if stored.UsedBytes == 0 {
		t.Error("usedDiskQuota is still zero after a delivery")
	}

	pop3 := tlsDial(t, server.Addr(maild.ListenerPOP3S))
	defer func() { drop(pop3.Close()) }()
	reader := bufio.NewReader(pop3)
	pop3Expect(t, reader, "banner")
	pop3Send(t, pop3, "USER %s", account.EmailAddress)
	pop3Expect(t, reader, "USER")
	pop3Send(t, pop3, "PASS %s", passphrase)
	pop3Expect(t, reader, "PASS")
	pop3Send(t, pop3, "STAT")
	stat := pop3Expect(t, reader, "STAT")
	if !strings.HasPrefix(stat, "+OK 1 ") {
		t.Errorf("STAT = %q, want one message", stat)
	}
	pop3Send(t, pop3, "RETR 1")
	pop3Expect(t, reader, "RETR")
	body := readUntilDot(t, reader)
	if !strings.Contains(body, "Subject: integration") || !strings.Contains(body, "hello") {
		t.Errorf("RETR returned %q, want the delivered message", body)
	}
	pop3Send(t, pop3, "QUIT")

	// Deleting the mailbox must actually remove the mail. The client's own
	// comment says "the purge behind it is asynchronous", but nothing in this
	// server ever purges: the maildir and its bytes outlive the account, so an
	// unbind with delete_mailboxes leaves every customer message on disk.
	if err := client.DestroyAccount(ctx, account.ID); err != nil {
		t.Fatalf("DestroyAccount: %v", err)
	}
	mailbox := maild.NewMaildir(dataDir).Path(account.ID)
	entries, err := os.ReadDir(filepath.Join(mailbox, "new"))
	if err == nil && len(entries) > 0 {
		t.Errorf("%d message(s) survive in %s after DestroyAccount", len(entries), mailbox)
	}
}

func selfSignedPair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: testHostname},
		DNSNames:     []string{testHostname},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	write := func(path, kind string, bytes []byte) {
		t.Helper()
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: bytes}), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(certFile, "CERTIFICATE", der)
	write(keyFile, "PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	return certFile, keyFile
}

func tlsDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	if addr == "" {
		t.Fatal("the POP3S listener reported no address")
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // a self-signed test certificate
	if err != nil {
		t.Fatalf("dial pop3s: %v", err)
	}
	drop(conn.SetDeadline(time.Now().Add(20 * time.Second)))
	return conn
}

func pop3Send(t *testing.T, conn net.Conn, format string, args ...any) {
	t.Helper()
	if _, err := fmt.Fprintf(conn, format+"\r\n", args...); err != nil {
		t.Fatalf("pop3 write: %v", err)
	}
}

func pop3Expect(t *testing.T, reader *bufio.Reader, context string) string {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("pop3 %s: %v", context, err)
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "+OK") {
		t.Fatalf("pop3 %s answered %q", context, line)
	}
	return line
}

func readUntilDot(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var builder strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("pop3 read message: %v", err)
		}
		if strings.TrimRight(line, "\r\n") == "." {
			return builder.String()
		}
		builder.WriteString(line)
	}
}

// TestZoneFileCarriesTheAutoconfigRecordsTheFacadePublishes is the one that
// fails.
//
// CONTRACT.md §4.3 and internal/mail/zonefile.go agree on where client
// autoconfiguration comes from: the facade parses these owners out of
// x:Domain.dnsZoneFile and publishes nothing it did not find there
// (parseAutoconfigRecords, called from engineRecords in domain.go). The binding
// turns publish_client_autoconfig on by default, so a server that renders only
// DKIM leaves the console reporting the feature as enabled while Thunderbird
// and Outlook get nothing.
func TestZoneFileCarriesTheAutoconfigRecordsTheFacadePublishes(t *testing.T) {
	_, client := liveServer(t)
	ctx := context.Background()

	domain, err := client.CreateDomain(ctx, stalwart.DomainInput{Name: testZone})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	got, err := client.GetDomain(ctx, domain.ID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	// The owners and rdata of CONTRACT.md §4.3, which parseAutoconfigRecords
	// accepts only when the target is exactly MAIL_HOSTNAME.
	for _, owner := range []string{
		"_submissions._tcp", "_imaps._tcp", "_pop3s._tcp",
		"_jmap._tcp", "_caldavs._tcp", "_carddavs._tcp",
		"autoconfig", "autodiscover",
	} {
		if !strings.Contains(got.DNSZoneFile, owner+"."+testZone) {
			t.Errorf("dnsZoneFile offers no %s record, so the facade publishes none", owner)
		}
	}
	t.Logf("dnsZoneFile the server rendered:\n%s", got.DNSZoneFile)
}

// TestSyntheticQueueMethodsTheFacadeReasonsAbout locks the two protocol answers
// the facade's own live suite asserts against a real Stalwart.
func TestUnknownMethodsAnswerTheWayTheFacadeExpects(t *testing.T) {
	_, client := liveServer(t)
	ctx := context.Background()

	// internal/mail/integration_test.go:255 requires a *forbidden* method error
	// here: per-domain delivery history is an edition refusal, not an unknown
	// method, and errors.go reasons about the two differently.
	_, err := client.Call(ctx, []stalwart.MethodCall{{Name: "x:Metric/query", Args: map[string]any{}, ID: "0"}})
	if !stalwart.IsForbidden(err) {
		t.Errorf("x:Metric/query = %v, want a forbidden method error", err)
	}

	// This one the client relies on in production: ListLists pages unfiltered
	// because a domainId filter must be refused with unsupportedFilter.
	_, filterErr := client.Call(ctx, []stalwart.MethodCall{{
		Name: "x:MailingList/query",
		Args: map[string]any{"filter": map[string]any{"domainId": "c"}},
		ID:   "0",
	}})
	kind, _ := stalwart.MethodErrorType(filterErr)
	if kind != stalwart.ErrorTypeUnsupportedFilter {
		t.Errorf("a domainId filter answered %v, want unsupportedFilter", filterErr)
	}
}
