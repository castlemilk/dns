package stalwart

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DKIM key management the facade asks Stalwart for. Durations are milliseconds
// (mail-facts.md §3). Rotation only happens with automatic DNS management,
// which this deployment does not use, so the intervals are the documented
// defaults rather than a promise: the keys stay active and the reconciler
// republishes whatever `stage: active` set the server reports.
const (
	dkimRotateAfterMillis = 7_776_000_000 // 90 d
	dkimRetireAfterMillis = 604_800_000   // 7 d
	dkimDeleteAfterMillis = 2_592_000_000 // 30 d
	dkimSelectorTemplate  = "v{version}-{algorithm}-{date-%Y%m%d}"
)

// Stalwart refuses `"description": ""` with `invalidProperties` / "Invalid
// value for property.": the property is optional, and an empty string is not a
// valid value for it. Display names are optional in this product, so an empty
// description is expressed the way JMAP expresses "no value": absent on create,
// null on update (which removes it).
func setDescription(create map[string]any, description string) {
	if description != "" {
		create["description"] = description
	}
}

// descriptionValue is the patch value for a description: the string, or null to
// clear it.
func descriptionValue(description string) any {
	if description == "" {
		return nil
	}
	return description
}

// Account reports the edition and the permissions the credential holds. It is
// the startup self-check: the facade compares the list against the names it
// needs and reports the difference rather than assuming.
func (c *Client) Account(ctx context.Context) (AccountInfo, error) {
	var payload struct {
		Permissions []string `json:"permissions"`
		Edition     string   `json:"edition"`
	}
	if err := c.getJSON(ctx, "/api/account", &payload); err != nil {
		return AccountInfo{}, err
	}
	return AccountInfo{Edition: payload.Edition, Permissions: payload.Permissions}, nil
}

// SystemSettings reads the x:SystemSettings singleton. A singleton is read with
// an explicit id: a bare /get returns an empty list.
func (c *Client) SystemSettings(ctx context.Context) (SystemSettings, error) {
	responses, err := c.Call(ctx, []MethodCall{{
		Name: "x:SystemSettings/get",
		Args: map[string]any{"ids": []string{"singleton"}},
		ID:   "0",
	}})
	if err != nil {
		return SystemSettings{}, err
	}
	var payload struct {
		List []struct {
			DefaultHostname string `json:"defaultHostname"`
			MailExchangers  map[string]struct {
				Hostname *string `json:"hostname"`
				Priority uint16  `json:"priority"`
			} `json:"mailExchangers"`
		} `json:"list"`
	}
	if err := decodeAt(responses, 0, &payload); err != nil {
		return SystemSettings{}, err
	}
	if len(payload.List) == 0 {
		return SystemSettings{}, fmt.Errorf("%w: the system settings singleton", ErrNotFound)
	}
	settings := SystemSettings{DefaultHostname: payload.List[0].DefaultHostname}
	for _, key := range sortedKeys(payload.List[0].MailExchangers) {
		entry := payload.List[0].MailExchangers[key]
		exchanger := MailExchanger{Priority: entry.Priority}
		if entry.Hostname != nil {
			exchanger.Hostname = *entry.Hostname
		} else {
			exchanger.Hostname = settings.DefaultHostname
		}
		settings.MailExchangers = append(settings.MailExchangers, exchanger)
	}
	return settings, nil
}

// domainProperties is the property set every domain read asks for. dnsZoneFile
// is the expensive one (the server renders BIND text), so the list query used
// by ListDomains asks for the cheap subset instead.
var domainProperties = []string{"name", "isEnabled", "description", "dnsZoneFile", "createdAt"}

var domainListProperties = []string{"name", "isEnabled", "description", "createdAt"}

type domainPayload struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"isEnabled"`
	DNSZoneFile string `json:"dnsZoneFile"`
	CreatedAt   string `json:"createdAt"`
}

func (p domainPayload) domain() Domain {
	return Domain{
		ID:          p.ID,
		Name:        p.Name,
		Description: p.Description,
		Enabled:     p.Enabled,
		DNSZoneFile: p.DNSZoneFile,
		CreatedAt:   parseTime(p.CreatedAt),
	}
}

// FindDomain looks one domain up by name. The `name` filter is listed by the
// server's own schema, so this is a server-side query.
func (c *Client) FindDomain(ctx context.Context, name string) (Domain, bool, error) {
	responses, err := c.Call(ctx, []MethodCall{
		{Name: "x:Domain/query", Args: map[string]any{"filter": map[string]any{"name": name}}, ID: "0"},
		{Name: "x:Domain/get", Args: map[string]any{
			"#ids":       resultReference("0", "x:Domain/query", "/ids"),
			"properties": domainProperties,
		}, ID: "1"},
	})
	if err != nil {
		return Domain{}, false, err
	}
	var payload struct {
		List []domainPayload `json:"list"`
	}
	if err := decodeAt(responses, 1, &payload); err != nil {
		return Domain{}, false, err
	}
	for _, entry := range payload.List {
		if strings.EqualFold(entry.Name, name) {
			return entry.domain(), true, nil
		}
	}
	return Domain{}, false, nil
}

// ListDomains pages through every domain. It exists for RebuildPlatformStore,
// which reconstructs the bindings from the engine after platform.db was lost.
func (c *Client) ListDomains(ctx context.Context) ([]Domain, error) {
	var domains []Domain
	for position := 0; ; position += MaxObjectsPerCall {
		responses, err := c.Call(ctx, []MethodCall{
			{Name: "x:Domain/query", Args: map[string]any{"limit": MaxObjectsPerCall, "position": position}, ID: "0"},
			{Name: "x:Domain/get", Args: map[string]any{
				"#ids":       resultReference("0", "x:Domain/query", "/ids"),
				"properties": domainListProperties,
			}, ID: "1"},
		})
		if err != nil {
			return nil, err
		}
		var query struct {
			IDs []string `json:"ids"`
		}
		if err := decodeAt(responses, 0, &query); err != nil {
			return nil, err
		}
		var payload struct {
			List []domainPayload `json:"list"`
		}
		if err := decodeAt(responses, 1, &payload); err != nil {
			return nil, err
		}
		for _, entry := range payload.List {
			domains = append(domains, entry.domain())
		}
		if len(query.IDs) < MaxObjectsPerCall {
			break
		}
	}
	return domains, nil
}

// CreateDomain creates a domain with automatic DKIM (both algorithms) and
// manual DNS and certificate management: this product publishes the records
// into its own zone store and issues no certificate for the customer name.
func (c *Client) CreateDomain(ctx context.Context, in DomainInput) (Domain, error) {
	create := map[string]any{
		"name":      in.Name,
		"isEnabled": true,
		"dkimManagement": map[string]any{
			"@type": "Automatic",
			"algorithms": map[string]bool{
				DkimAlgorithmEd25519: true,
				DkimAlgorithmRSA:     true,
			},
			"selectorTemplate": dkimSelectorTemplate,
			"rotateAfter":      dkimRotateAfterMillis,
			"retireAfter":      dkimRetireAfterMillis,
			"deleteAfter":      dkimDeleteAfterMillis,
		},
		"dnsManagement":         map[string]any{"@type": "Manual"},
		"certificateManagement": map[string]any{"@type": "Manual"},
		"subAddressing":         map[string]any{"@type": "Enabled"},
		"reportAddressUri":      in.ReportAddressURI,
		"allowRelaying":         false,
		"aliases":               map[string]any{},
		"catchAllAddress":       nil,
	}
	setDescription(create, in.Description)
	responses, err := c.Call(ctx, []MethodCall{{
		Name: "x:Domain/set",
		Args: map[string]any{"create": map[string]any{"d1": create}},
		ID:   "0",
	}})
	if err != nil {
		return Domain{}, err
	}
	id, err := createdID(responses, 0, "x:Domain/set", "d1")
	if err != nil {
		return Domain{}, err
	}
	return c.GetDomain(ctx, id)
}

// GetDomain reads one domain including its rendered dnsZoneFile.
func (c *Client) GetDomain(ctx context.Context, id string) (Domain, error) {
	responses, err := c.Call(ctx, []MethodCall{{
		Name: "x:Domain/get",
		Args: map[string]any{"ids": []string{id}, "properties": domainProperties},
		ID:   "0",
	}})
	if err != nil {
		return Domain{}, err
	}
	var payload struct {
		List []domainPayload `json:"list"`
	}
	if err := decodeAt(responses, 0, &payload); err != nil {
		return Domain{}, err
	}
	if len(payload.List) == 0 {
		return Domain{}, fmt.Errorf("%w: domain", ErrNotFound)
	}
	return payload.List[0].domain(), nil
}

// UpdateDomain applies a partial update.
func (c *Client) UpdateDomain(ctx context.Context, id string, patch DomainPatch) error {
	update := map[string]any{}
	if patch.Description != nil {
		update["description"] = *patch.Description
	}
	if patch.ReportAddressURI != nil {
		update["reportAddressUri"] = *patch.ReportAddressURI
	}
	if patch.Enabled != nil {
		update["isEnabled"] = *patch.Enabled
	}
	if len(update) == 0 {
		return nil
	}
	responses, err := c.Call(ctx, []MethodCall{{
		Name: "x:Domain/set",
		Args: map[string]any{"update": map[string]any{id: update}},
		ID:   "0",
	}})
	if err != nil {
		return err
	}
	return checkSet(responses, 0, "x:Domain/set")
}

// DestroyDomain removes a domain. It fails with objectIsLinked while accounts,
// mailing lists or DKIM signatures still exist.
func (c *Client) DestroyDomain(ctx context.Context, id string) error {
	return c.destroy(ctx, "x:Domain/set", id)
}

// ListDkim reads a domain's DKIM signatures. The domainId filter is listed by
// the schema for this object, so it is a server-side query.
func (c *Client) ListDkim(ctx context.Context, domainID string) ([]DkimSignature, error) {
	responses, err := c.Call(ctx, []MethodCall{
		{Name: "x:DkimSignature/query", Args: map[string]any{"filter": map[string]any{"domainId": domainID}}, ID: "0"},
		{Name: "x:DkimSignature/get", Args: map[string]any{
			"#ids":       resultReference("0", "x:DkimSignature/query", "/ids"),
			"properties": []string{"selector", "@type", "stage", "createdAt", "publicKey"},
		}, ID: "1"},
	})
	if err != nil {
		return nil, err
	}
	var payload struct {
		List []struct {
			ID        string `json:"id"`
			Selector  string `json:"selector"`
			Algorithm string `json:"@type"`
			Stage     string `json:"stage"`
			PublicKey string `json:"publicKey"`
			CreatedAt string `json:"createdAt"`
		} `json:"list"`
	}
	if err := decodeAt(responses, 1, &payload); err != nil {
		return nil, err
	}
	signatures := make([]DkimSignature, 0, len(payload.List))
	for _, entry := range payload.List {
		signatures = append(signatures, DkimSignature{
			ID:        entry.ID,
			Selector:  entry.Selector,
			Algorithm: entry.Algorithm,
			Stage:     entry.Stage,
			PublicKey: entry.PublicKey,
			CreatedAt: parseTime(entry.CreatedAt),
		})
	}
	sort.Slice(signatures, func(i, j int) bool { return signatures[i].Selector < signatures[j].Selector })
	return signatures, nil
}

// DestroyDkim removes one signature.
func (c *Client) DestroyDkim(ctx context.Context, id string) error {
	return c.destroy(ctx, "x:DkimSignature/set", id)
}

type accountPayload struct {
	ID           string `json:"id"`
	EmailAddress string `json:"emailAddress"`
	Description  string `json:"description"`
	DomainID     string `json:"domainId"`
	Quotas       struct {
		MaxDiskQuota uint64 `json:"maxDiskQuota"`
	} `json:"quotas"`
	UsedDiskQuota uint64 `json:"usedDiskQuota"`
	Aliases       map[string]struct {
		Name        string `json:"name"`
		DomainID    string `json:"domainId"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
	} `json:"aliases"`
	CreatedAt string `json:"createdAt"`
}

func (p accountPayload) account() Account {
	local, _, _ := strings.Cut(p.EmailAddress, "@")
	value := Account{
		ID:           p.ID,
		EmailAddress: p.EmailAddress,
		LocalPart:    local,
		DomainID:     p.DomainID,
		Description:  p.Description,
		QuotaBytes:   p.Quotas.MaxDiskQuota,
		UsedBytes:    p.UsedDiskQuota,
		CreatedAt:    parseTime(p.CreatedAt),
	}
	for _, key := range sortedKeys(p.Aliases) {
		entry := p.Aliases[key]
		value.Aliases = append(value.Aliases, Alias{
			Name:        entry.Name,
			DomainID:    entry.DomainID,
			Description: entry.Description,
			Enabled:     entry.Enabled,
		})
	}
	sort.Slice(value.Aliases, func(i, j int) bool { return value.Aliases[i].Name < value.Aliases[j].Name })
	return value
}

var accountProperties = []string{"emailAddress", "description", "domainId", "quotas", "usedDiskQuota", "aliases", "createdAt"}

// ListAccounts reads a domain's mailboxes with their live usage. The
// `@type`/`domainId` filter pair is probe-verified for this object.
func (c *Client) ListAccounts(ctx context.Context, domainID string) ([]Account, error) {
	responses, err := c.Call(ctx, []MethodCall{
		{Name: "x:Account/query", Args: map[string]any{
			"filter": map[string]any{"@type": "User", "domainId": domainID},
			"limit":  MaxObjectsPerCall,
		}, ID: "0"},
		{Name: "x:Account/get", Args: map[string]any{
			"#ids":       resultReference("0", "x:Account/query", "/ids"),
			"properties": accountProperties,
		}, ID: "1"},
	})
	if err != nil {
		return nil, err
	}
	var payload struct {
		List []accountPayload `json:"list"`
	}
	if err := decodeAt(responses, 1, &payload); err != nil {
		return nil, err
	}
	accounts := make([]Account, 0, len(payload.List))
	for _, entry := range payload.List {
		accounts = append(accounts, entry.account())
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].EmailAddress < accounts[j].EmailAddress })
	return accounts, nil
}

// CreateAccount creates one mailbox. The credential travels once, in this
// request, and is never held anywhere else in this package.
func (c *Client) CreateAccount(ctx context.Context, in AccountInput) (Account, error) {
	create := map[string]any{
		"@type":    "User",
		"name":     in.LocalPart,
		"domainId": in.DomainID,
		"credentials": map[string]any{
			"0": map[string]any{"@type": "Password", "secret": in.Secret},
		},
		"roles":            map[string]any{"@type": "User"},
		"permissions":      map[string]any{"@type": "Inherit"},
		"quotas":           map[string]any{"maxDiskQuota": in.QuotaBytes},
		"aliases":          map[string]any{},
		"locale":           "en-US",
		"memberGroupIds":   map[string]any{},
		"encryptionAtRest": map[string]any{"@type": "Disabled"},
	}
	setDescription(create, in.Description)
	responses, err := c.Call(ctx, []MethodCall{{
		Name: "x:Account/set",
		Args: map[string]any{"create": map[string]any{"u1": create}},
		ID:   "0",
	}})
	if err != nil {
		return Account{}, err
	}
	id, err := createdID(responses, 0, "x:Account/set", "u1")
	if err != nil {
		return Account{}, err
	}
	return c.GetAccount(ctx, id)
}

// GetAccount reads one mailbox.
func (c *Client) GetAccount(ctx context.Context, id string) (Account, error) {
	responses, err := c.Call(ctx, []MethodCall{{
		Name: "x:Account/get",
		Args: map[string]any{"ids": []string{id}, "properties": accountProperties},
		ID:   "0",
	}})
	if err != nil {
		return Account{}, err
	}
	var payload struct {
		List []accountPayload `json:"list"`
	}
	if err := decodeAt(responses, 0, &payload); err != nil {
		return Account{}, err
	}
	if len(payload.List) == 0 {
		return Account{}, fmt.Errorf("%w: account", ErrNotFound)
	}
	return payload.List[0].account(), nil
}

// UpdateAccount applies a partial update: quota, display name, aliases or a new
// credential.
func (c *Client) UpdateAccount(ctx context.Context, id string, patch AccountPatch) error {
	update := map[string]any{}
	if patch.Description != nil {
		update["description"] = descriptionValue(*patch.Description)
	}
	if patch.QuotaBytes != nil {
		update["quotas"] = map[string]any{"maxDiskQuota": *patch.QuotaBytes}
	}
	if patch.Aliases != nil {
		// x:Account/set replaces the whole aliases map rather than merging into
		// it, and every caller of this patch is a read-modify-write over the
		// list this client just read. So each alias is written back exactly as
		// it was read: an alias an operator disabled in the mail server's own UI
		// stays disabled, and its description survives, instead of both being
		// silently reset by an unrelated "add a forwarder" in the console.
		aliases := map[string]any{}
		for index, alias := range *patch.Aliases {
			aliases[fmt.Sprint(index)] = map[string]any{
				"name":        alias.Name,
				"domainId":    alias.DomainID,
				"description": descriptionValue(alias.Description),
				"enabled":     alias.Enabled,
			}
		}
		update["aliases"] = aliases
	}
	if patch.Secret != nil {
		update["credentials"] = map[string]any{
			"0": map[string]any{"@type": "Password", "secret": *patch.Secret},
		}
	}
	if len(update) == 0 {
		return nil
	}
	responses, err := c.Call(ctx, []MethodCall{{
		Name: "x:Account/set",
		Args: map[string]any{"update": map[string]any{id: update}},
		ID:   "0",
	}})
	if err != nil {
		return err
	}
	return checkSet(responses, 0, "x:Account/set")
}

// DestroyAccount removes one mailbox. The purge behind it is asynchronous.
func (c *Client) DestroyAccount(ctx context.Context, id string) error {
	return c.destroy(ctx, "x:Account/set", id)
}

// ListLists returns the mailing lists of one domain, or of every domain when
// domainID is empty.
//
// x:MailingList/query supports only the `text` and `memberTenantId` filters —
// a domainId filter answers unsupportedFilter — so the query is unfiltered and
// paged, and the domain match happens here.
func (c *Client) ListLists(ctx context.Context, domainID string) ([]MailingList, error) {
	var lists []MailingList
	for position := 0; ; position += MaxObjectsPerCall {
		responses, err := c.Call(ctx, []MethodCall{
			{Name: "x:MailingList/query", Args: map[string]any{"limit": MaxObjectsPerCall, "position": position}, ID: "0"},
			{Name: "x:MailingList/get", Args: map[string]any{
				"#ids":       resultReference("0", "x:MailingList/query", "/ids"),
				"properties": []string{"name", "domainId", "description", "recipients", "emailAddress"},
			}, ID: "1"},
		})
		if err != nil {
			return nil, err
		}
		var query struct {
			IDs []string `json:"ids"`
		}
		if err := decodeAt(responses, 0, &query); err != nil {
			return nil, err
		}
		var payload struct {
			List []struct {
				ID           string          `json:"id"`
				Name         string          `json:"name"`
				DomainID     string          `json:"domainId"`
				Description  string          `json:"description"`
				EmailAddress string          `json:"emailAddress"`
				Recipients   map[string]bool `json:"recipients"`
			} `json:"list"`
		}
		if err := decodeAt(responses, 1, &payload); err != nil {
			return nil, err
		}
		for _, entry := range payload.List {
			if domainID != "" && entry.DomainID != domainID {
				continue
			}
			list := MailingList{
				ID:           entry.ID,
				Name:         entry.Name,
				DomainID:     entry.DomainID,
				Description:  entry.Description,
				EmailAddress: entry.EmailAddress,
			}
			for address, enabled := range entry.Recipients {
				if enabled {
					list.Recipients = append(list.Recipients, address)
				}
			}
			sort.Strings(list.Recipients)
			lists = append(lists, list)
		}
		if len(query.IDs) < MaxObjectsPerCall {
			break
		}
	}
	sort.Slice(lists, func(i, j int) bool { return lists[i].EmailAddress < lists[j].EmailAddress })
	return lists, nil
}

// CreateList creates a mailing list, the object behind a forwarder with an
// external target or several targets.
func (c *Client) CreateList(ctx context.Context, in ListInput) (MailingList, error) {
	recipients := make(map[string]bool, len(in.Recipients))
	for _, address := range in.Recipients {
		recipients[address] = true
	}
	create := map[string]any{
		"name":       in.LocalPart,
		"domainId":   in.DomainID,
		"recipients": recipients,
		"aliases":    map[string]any{},
	}
	setDescription(create, in.Description)
	responses, err := c.Call(ctx, []MethodCall{{
		Name: "x:MailingList/set",
		Args: map[string]any{"create": map[string]any{"l1": create}},
		ID:   "0",
	}})
	if err != nil {
		return MailingList{}, err
	}
	id, err := createdID(responses, 0, "x:MailingList/set", "l1")
	if err != nil {
		return MailingList{}, err
	}
	list := MailingList{
		ID:          id,
		Name:        in.LocalPart,
		DomainID:    in.DomainID,
		Description: in.Description,
		Recipients:  append([]string(nil), in.Recipients...),
	}
	sort.Strings(list.Recipients)
	return list, nil
}

// DestroyList removes one mailing list.
func (c *Client) DestroyList(ctx context.Context, id string) error {
	return c.destroy(ctx, "x:MailingList/set", id)
}

// QueryQueue returns the queued messages that belong to zoneName.
//
// The server's `returnPath`/`to` filters are substring matches, and a forwarder
// fan-out carries the EXTERNAL target as the recipient with the customer's
// address only in `orcpt`, so a server-side filter would both over- and
// under-match. The query is therefore unfiltered and attribution happens here:
// a message belongs to the zone when the exact domain of returnPath, of any
// recipient address, or of any recipient's orcpt equals the zone name.
//
// complete is false when the page came back full: attribution is then
// incomplete and the caller must say "not available" rather than show a count
// that is silently a lower bound.
func (c *Client) QueryQueue(ctx context.Context, zoneName string, limit int) ([]QueuedMessage, bool, error) {
	if limit <= 0 || limit > MaxObjectsPerCall {
		limit = MaxObjectsPerCall
	}
	responses, err := c.Call(ctx, []MethodCall{
		{Name: "x:QueuedMessage/query", Args: map[string]any{"limit": MaxObjectsPerCall}, ID: "0"},
		{Name: "x:QueuedMessage/get", Args: map[string]any{
			"#ids":       resultReference("0", "x:QueuedMessage/query", "/ids"),
			"properties": []string{"returnPath", "recipients", "createdAt", "nextRetry"},
		}, ID: "1"},
	})
	if err != nil {
		return nil, false, err
	}
	var query struct {
		IDs []string `json:"ids"`
	}
	if err := decodeAt(responses, 0, &query); err != nil {
		return nil, false, err
	}
	var payload struct {
		List []struct {
			ID         string `json:"id"`
			ReturnPath string `json:"returnPath"`
			CreatedAt  string `json:"createdAt"`
			NextRetry  string `json:"nextRetry"`
			Recipients map[string]struct {
				ORCPT      *string `json:"orcpt"`
				QueueName  string  `json:"queueName"`
				RetryCount uint32  `json:"retryCount"`
				RetryDue   string  `json:"retryDue"`
				Status     struct {
					Type string `json:"@type"`
				} `json:"status"`
			} `json:"recipients"`
		} `json:"list"`
	}
	if err := decodeAt(responses, 1, &payload); err != nil {
		return nil, false, err
	}

	complete := len(query.IDs) < MaxObjectsPerCall
	messages := make([]QueuedMessage, 0, len(payload.List))
	for _, entry := range payload.List {
		message := QueuedMessage{
			ID:         entry.ID,
			ReturnPath: entry.ReturnPath,
			CreatedAt:  parseTime(entry.CreatedAt),
			NextRetry:  parseTime(entry.NextRetry),
		}
		for _, address := range sortedKeys(entry.Recipients) {
			recipient := entry.Recipients[address]
			mapped := QueuedRecipient{
				Address:    address,
				QueueName:  recipient.QueueName,
				RetryCount: recipient.RetryCount,
				RetryDue:   parseTime(recipient.RetryDue),
				Status:     recipient.Status.Type,
			}
			if recipient.ORCPT != nil {
				mapped.ORCPT = normalizeORCPT(*recipient.ORCPT)
			}
			message.Recipients = append(message.Recipients, mapped)
		}
		if !BelongsToZone(message, zoneName) {
			continue
		}
		messages = append(messages, message)
		if len(messages) >= limit {
			break
		}
	}
	return messages, complete, nil
}

// BelongsToZone reports whether a queued message is attributable to zoneName.
// It is exported because the facade's tests and the fake engine must apply the
// identical rule; a second implementation would drift.
func BelongsToZone(message QueuedMessage, zoneName string) bool {
	if zoneName == "" {
		return true
	}
	if addressDomain(message.ReturnPath) == strings.ToLower(zoneName) {
		return true
	}
	for _, recipient := range message.Recipients {
		if addressDomain(recipient.Address) == strings.ToLower(zoneName) {
			return true
		}
		if addressDomain(recipient.ORCPT) == strings.ToLower(zoneName) {
			return true
		}
	}
	return false
}

// normalizeORCPT strips the SMTP ORCPT type prefix ("rfc822;user@host").
func normalizeORCPT(value string) string {
	if _, address, found := strings.Cut(value, ";"); found {
		return strings.TrimSpace(address)
	}
	return strings.TrimSpace(value)
}

func addressDomain(address string) string {
	_, domain, found := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
	if !found {
		return ""
	}
	return strings.Trim(domain, "<> .")
}

// destroy is the shared /set destroy path: a NotFound is success, because every
// caller here is making the object's absence true.
func (c *Client) destroy(ctx context.Context, method, id string) error {
	responses, err := c.Call(ctx, []MethodCall{{
		Name: method,
		Args: map[string]any{"destroy": []string{id}},
		ID:   "0",
	}})
	if err != nil {
		return err
	}
	if err := checkSet(responses, 0, method); err != nil {
		if IsMissing(err) {
			return nil
		}
		return err
	}
	return nil
}

// resultReference builds a JMAP back-reference so a query and the get that
// consumes its ids travel in one request.
func resultReference(resultOf, name, path string) map[string]any {
	return map[string]any{"resultOf": resultOf, "name": name, "path": path}
}

func decodeAt(responses []MethodResponse, index int, out any) error {
	if index >= len(responses) {
		return fmt.Errorf("%w: the server answered %d of %d method calls", ErrTransport, len(responses), index+1)
	}
	if err := json.Unmarshal(responses[index].Args, out); err != nil {
		return fmt.Errorf("%w: %s answered an unexpected shape", ErrTransport, responses[index].Name)
	}
	return nil
}

// setResponse is the shape of every /set answer.
type setResponse struct {
	Created map[string]struct {
		ID string `json:"id"`
	} `json:"created"`
	Updated      map[string]json.RawMessage `json:"updated"`
	Destroyed    []string                   `json:"destroyed"`
	NotCreated   map[string]setErrorPayload `json:"notCreated"`
	NotUpdated   map[string]setErrorPayload `json:"notUpdated"`
	NotDestroyed map[string]setErrorPayload `json:"notDestroyed"`
}

type setErrorPayload struct {
	Type          string   `json:"type"`
	Description   string   `json:"description"`
	Properties    []string `json:"properties"`
	LinkedObjects []struct {
		Object string `json:"object"`
		ID     string `json:"id"`
	} `json:"linkedObjects"`
}

func (p setErrorPayload) err(method, operation, key string) *SetError {
	value := &SetError{
		Method:      method,
		Operation:   operation,
		Key:         key,
		Type:        p.Type,
		Description: p.Description,
		Properties:  append([]string(nil), p.Properties...),
	}
	for _, linked := range p.LinkedObjects {
		value.LinkedObjects = append(value.LinkedObjects, LinkedObject{Object: linked.Object, ID: linked.ID})
	}
	return value
}

// checkSet turns the first not* entry of a /set answer into a *SetError.
func checkSet(responses []MethodResponse, index int, method string) error {
	var payload setResponse
	if err := decodeAt(responses, index, &payload); err != nil {
		return err
	}
	for _, key := range sortedKeys(payload.NotCreated) {
		return payload.NotCreated[key].err(method, "create", key)
	}
	for _, key := range sortedKeys(payload.NotUpdated) {
		return payload.NotUpdated[key].err(method, "update", key)
	}
	for _, key := range sortedKeys(payload.NotDestroyed) {
		return payload.NotDestroyed[key].err(method, "destroy", key)
	}
	return nil
}

// createdID reads the id assigned to one creation id.
func createdID(responses []MethodResponse, index int, method, creationID string) (string, error) {
	if err := checkSet(responses, index, method); err != nil {
		return "", err
	}
	var payload setResponse
	if err := decodeAt(responses, index, &payload); err != nil {
		return "", err
	}
	entry, ok := payload.Created[creationID]
	if !ok || entry.ID == "" {
		return "", fmt.Errorf("%w: %s reported neither a created object nor an error", ErrTransport, method)
	}
	return entry.ID, nil
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
