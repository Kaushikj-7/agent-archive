package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func dedicatedFixture(t *testing.T) (Env, string, *cloudflaretest.Server, *fakeKeychain) {
	t.Helper()
	env, home, _ := pairingSourceFixture(t)
	cfg, _, err := config.Load(home)
	must(t, err)
	cfg.Storage = credentials.Config{Provider: credentials.ProviderR2, Bucket: "synthetic", Prefix: "archive/", R2AccountID: cloudflaretest.AccountID, R2CredentialRef: "main"}
	kc := newFakeKeychain()
	must(t, kc.Save(context.Background(), "main", credentials.R2Credentials{AccessKeyID: strings.Repeat("e", 32), SecretAccessKey: "object-canary"}))
	must(t, config.Save(home, cfg))
	env.Credentials = func() (credentials.CredentialStore, error) { return kc, nil }
	cf := cloudflaretest.New(t, bootstrapCanary)
	env.Cloudflare = func(token string) cloudflare.API {
		return cloudflare.New(token, cloudflare.Options{BaseURL: cf.URL + "/client/v4", Sleep: func(context.Context, time.Duration) error { return nil }})
	}
	env.Pause = func(time.Duration) {}
	env.LookupEnv = func(key string) (string, bool) {
		return "", false
	}
	env.UnsetEnv = func(string) error { return nil }
	return env, home, cf, kc
}

func fixtureIssuer(t *testing.T, env Env, home string) *keyIssuer {
	t.Helper()
	cfg, _, err := config.Load(home)
	must(t, err)
	i, err := newKeyIssuer(home, cfg, env, newPrompter(strings.NewReader(""), &bytes.Buffer{}), env.cloudflareAPI(bootstrapCanary))
	must(t, err)
	t.Cleanup(i.api.Discard)
	return i
}

func TestDedicatedCreationReservationAndSecretFreeLedger(t *testing.T) {
	env, home, cf, kc := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	release, err := local.NamedLock(home, "issued.lock")
	must(t, err)
	defer release()
	s, key, err := i.create(issuance.Precreated)
	must(t, err)
	identity, ok := cloudflare.ParseProviderName(cf.Tokens()[0].Name)
	if !ok || identity.SlotID != s.SlotID || identity.RecipientID != s.RecipientID || identity.IssuerID != i.cfg.MachineID {
		t.Fatal("immutable name mismatch")
	}
	selected, loaded, err := reserveSpare(home, i.cfg, strings.Repeat("b", 32), "laptop", env.now().Add(time.Hour), env)
	must(t, err)
	if selected.SlotID != s.SlotID || loaded != key {
		t.Fatal("wrong reserved secret")
	}
	second, _, err := reserveSpare(home, i.cfg, strings.Repeat("c", 32), "desktop", env.now().Add(time.Hour), env)
	must(t, err)
	if second.SlotID != "" {
		t.Fatal("reserved twice")
	}
	selected.State = issuance.DeliveryIntent
	must(t, issuance.Save(home, selected))
	releaseUntouchedSlot(home, &selected, nil)
	discardDeliveredSecret(home, &selected, env, &bytes.Buffer{})
	if _, err = kc.Load(context.Background(), s.SecretRef); err == nil {
		t.Fatal("delivered spare secret retained")
	}
	data, err := os.ReadFile(filepath.Join(home, "issued", "slot-"+s.SlotID+".json"))
	must(t, err)
	for _, secret := range []string{key.SecretAccessKey, bootstrapCanary, cf.Tokens()[0].Value} {
		if strings.Contains(string(data), secret) {
			t.Fatal("ledger leaked a secret")
		}
	}
	slots, err := issuance.List(home)
	must(t, err)
	if slots[0].State != issuance.DeliveryIntent {
		t.Fatal("uncertain exposure recycled")
	}
}

func TestDedicatedLostResponseReconcilesUniqueSlot(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	client := i.api
	i.api = &trackedInventoryAPI{API: client, InventoryAPI: client.(cloudflare.InventoryAPI), create: func(ctx context.Context, a string, s cloudflare.TokenSpec) (cloudflare.Token, error) {
		_, err := client.CreateToken(ctx, a, s)
		must(t, err)
		return cloudflare.Token{}, errors.New("lost response canary")
	}}
	s, _, err := i.create(issuance.Fresh)
	if err == nil || s.State != issuance.Deleted || len(cf.Live()) != 0 || len(cf.Tokens()) != 1 {
		t.Fatalf("lost creation not cleaned: state %s live %d", s.State, len(cf.Live()))
	}
	// Creation intent alone is enough to reconcile a crash without the one-time value.
	s, err = issuance.New(i.cfg.MachineID, i.cfg.DestinationID(), i.account, i.bucket, i.group, issuance.Fresh, env.now())
	must(t, err)
	must(t, issuance.Save(home, s))
	resource, err := cloudflare.BucketResource(i.account, i.bucket)
	must(t, err)
	_, err = client.CreateToken(context.Background(), i.account, cloudflare.TokenSpec{Name: s.ProviderName, Policies: []cloudflare.Policy{{PermissionGroupIDs: []string{i.group}, Resources: map[string]string{resource: "*"}}}})
	must(t, err)
	must(t, i.reconcile())
	if len(cf.Live()) != 0 {
		t.Fatal("crash orphan survived reconciliation")
	}
}

type trackedInventoryAPI struct {
	cloudflare.API
	cloudflare.InventoryAPI
	create func(context.Context, string, cloudflare.TokenSpec) (cloudflare.Token, error)
}

func (a *trackedInventoryAPI) CreateToken(ctx context.Context, account string, spec cloudflare.TokenSpec) (cloudflare.Token, error) {
	return a.create(ctx, account, spec)
}

func TestDedicatedUnknownCleanupRemainsPending(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	client := i.api
	i.api = &trackedInventoryAPI{API: client, InventoryAPI: client.(cloudflare.InventoryAPI), create: func(context.Context, string, cloudflare.TokenSpec) (cloudflare.Token, error) {
		return cloudflare.Token{}, errors.New("lost")
	}}
	s, _, err := i.create(issuance.Fresh)
	if err == nil || s.State != issuance.CleanupPending || len(cf.Tokens()) != 0 {
		t.Fatal("unknown creation treated as known absent")
	}
	slots, err := issuance.List(home)
	must(t, err)
	if slots[0].State != issuance.CleanupPending {
		t.Fatal("uncertainty lost")
	}
}

func TestDedicatedAddFreshThenSpareAndUncertainDelivery(t *testing.T) {
	env, home, cf, kc := dedicatedFixture(t)
	env.LookupEnv = func(key string) (string, bool) {
		if key == "CLOUDFLARE_API_TOKEN" {
			return bootstrapCanary, true
		}
		return "", false
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("fresh add %d %s", code, &errOut)
	}
	if len(cf.Tokens()) != 3 || len(kc.items) != 3 {
		t.Fatalf("fresh+two spares, secrets %d tokens %d", len(kc.items), len(cf.Tokens()))
	}
	bundle, code := pairingPieces(out.String())
	payload, err := pairing.Open(bundle, code, env.now())
	must(t, err)
	if payload.Kind != config.MachineAssignmentR2Own || !pairing.ValidID(payload.SlotID) {
		t.Fatal("dedicated provenance absent")
	}
	env.LookupEnv = func(string) (string, bool) { return "", false }
	out.Reset()
	errOut.Reset()
	path := filepath.Join(t.TempDir(), "existing")
	must(t, os.WriteFile(path, []byte("preserve"), 0600))
	if Run([]string{"machines", "add", "--yes", "--name", "desktop", "--file", path}, strings.NewReader(""), &out, &errOut, env) != 1 {
		t.Fatal("uncertain file delivery accepted")
	}
	if len(cf.Tokens()) != 3 || len(kc.items) != 2 {
		t.Fatalf("spare reused or delivered secret retained: tokens=%d secrets=%d stderr=%s", len(cf.Tokens()), len(kc.items), &errOut)
	}
	slots, err := issuance.List(home)
	must(t, err)
	intents := 0
	for _, s := range slots {
		if s.State == issuance.DeliveryIntent {
			intents++
		}
	}
	if intents != 1 {
		t.Fatal("missing delivery intent")
	}
	cfg, _, err := config.Load(home)
	must(t, err)
	if len(cfg.SpareCredentialRefs) != 1 {
		t.Fatal("advisory index stale")
	}
}

func TestDedicatedNoImplicitSharingAndCorruptLedger(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	var out bytes.Buffer
	if Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &out, env) != 1 || !strings.Contains(out.String(), "no key was shared") || len(cf.Tokens()) != 0 {
		t.Fatal("implicit sharing")
	}
	must(t, os.MkdirAll(filepath.Join(home, "issued"), 0700))
	must(t, os.WriteFile(filepath.Join(home, "issued", "slot-"+strings.Repeat("d", 32)+".json"), []byte("{}"), 0600))
	out.Reset()
	if Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &out, env) != 1 || !strings.Contains(out.String(), "spares withheld") {
		t.Fatal("corrupt ledger granted eligibility")
	}
}

func TestDedicatedIssuanceLockExcludesSecondReservation(t *testing.T) {
	env, home, _, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	first, err := local.NamedLock(home, "issued.lock")
	must(t, err)
	_, _, err = i.create(issuance.Precreated)
	must(t, err)
	done := make(chan error, 1)
	go func() {
		unlock, err := local.NamedLock(home, "issued.lock")
		if err == nil {
			unlock()
		}
		done <- err
	}()
	if err = <-done; !errors.Is(err, local.ErrBusy) {
		t.Fatal("second process admitted")
	}
	first()
	release, err := local.NamedLock(home, "issued.lock")
	must(t, err)
	defer release()
	slot, _, err := reserveSpare(home, i.cfg, strings.Repeat("b", 32), "laptop", env.now().Add(time.Hour), env)
	must(t, err)
	if slot.SlotID == "" {
		t.Fatal("first reservation missing")
	}
	slot, _, err = reserveSpare(home, i.cfg, strings.Repeat("c", 32), "desktop", env.now().Add(time.Hour), env)
	must(t, err)
	if slot.SlotID != "" {
		t.Fatal("same spare handed out twice")
	}
}

func TestDedicatedVerificationFailureCleansBeforeExposure(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	i.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, errors.New("storage refused") }
	s, _, err := i.create(issuance.Fresh)
	if err == nil || s.State != issuance.Deleted || len(cf.Live()) != 0 {
		t.Fatal("failed pre-exposure key left active")
	}
}

func TestDedicatedRefillFailureKeepsDeliveredBundleValid(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	original := env.Cloudflare
	calls := 0
	env.Cloudflare = func(token string) cloudflare.API {
		client := original(token)
		return &trackedInventoryAPI{API: client, InventoryAPI: client.(cloudflare.InventoryAPI), create: func(ctx context.Context, a string, s cloudflare.TokenSpec) (cloudflare.Token, error) {
			calls++
			if calls > 1 {
				return cloudflare.Token{}, &cloudflare.Error{Status: 403}
			}
			return client.CreateToken(ctx, a, s)
		}}
	}
	env.LookupEnv = func(key string) (string, bool) {
		if key == "CLOUDFLARE_API_TOKEN" {
			return bootstrapCanary, true
		}
		return "", false
	}
	var out, errOut bytes.Buffer
	if Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &errOut, env) != 0 || !strings.Contains(errOut.String(), "This pairing is ready") {
		t.Fatal("refill invalidated delivered pairing")
	}
	bundle, code := pairingPieces(out.String())
	payload, err := pairing.Open(bundle, code, env.now())
	must(t, err)
	if len(cf.Live()) != 1 || payload.AccessKeyID != cf.Live()[0].ID {
		t.Fatal("delivered key deleted after independent refill failure")
	}
	slots, err := issuance.List(home)
	must(t, err)
	delivered := 0
	for _, s := range slots {
		if s.State == issuance.Delivered {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatal("missing delivered lineage")
	}
}

func pairingPieces(output string) (bundle, code string) {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, "aa-pair1:") {
			bundle = line
		}
		if value, ok := strings.CutPrefix(line, "Pairing code (deliver separately): "); ok {
			code = value
		}
	}
	return bundle, code
}

func TestDedicatedFreshRefusalFallsBackToExistingSpare(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	release, err := local.NamedLock(home, "issued.lock")
	must(t, err)
	s, _, err := i.create(issuance.Precreated)
	must(t, err)
	release()
	original := env.Cloudflare
	env.Cloudflare = func(token string) cloudflare.API {
		client := original(token)
		return &trackedInventoryAPI{API: client, InventoryAPI: client.(cloudflare.InventoryAPI), create: func(context.Context, string, cloudflare.TokenSpec) (cloudflare.Token, error) {
			return cloudflare.Token{}, &cloudflare.Error{Status: 403}
		}}
	}
	env.LookupEnv = func(key string) (string, bool) {
		if key == "CLOUDFLARE_API_TOKEN" {
			return bootstrapCanary, true
		}
		return "", false
	}
	var out, errOut bytes.Buffer
	if Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &errOut, env) != 0 {
		t.Fatalf("spare fallback failed: %s", &errOut)
	}
	bundle, code := pairingPieces(out.String())
	payload, err := pairing.Open(bundle, code, env.now())
	must(t, err)
	if payload.SlotID != s.SlotID || len(cf.Tokens()) != 1 || !strings.Contains(out.String(), "eligible spare instead") {
		t.Fatal("fresh refusal lost valid spare")
	}
}

func TestDedicatedSparesZeroAndFlagBounds(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	var out bytes.Buffer
	if Run([]string{"machines", "add", "--yes", "--name", "laptop", "--spares=-1"}, strings.NewReader(""), &out, &out, env) != 2 {
		t.Fatal("negative spare flag accepted")
	}
	env.LookupEnv = func(key string) (string, bool) {
		if key == "CLOUDFLARE_API_TOKEN" {
			return bootstrapCanary, true
		}
		return "", false
	}
	out.Reset()
	if Run([]string{"machines", "add", "--yes", "--name", "laptop", "--spares=0"}, strings.NewReader(""), &out, &out, env) != 0 {
		t.Fatal("zero spare issuance failed")
	}
	cfg, _, err := config.Load(home)
	must(t, err)
	if cfg.SpareTarget() != 0 || len(cf.Tokens()) != 1 || len(cfg.SpareCredentialRefs) != 0 {
		t.Fatal("zero policy did not disable refill")
	}
}

func TestDedicatedPropagationRetriesBeforeExposure(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	original := env.OpenStore
	calls, pauses := 0, 0
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		calls++
		if calls <= 2 {
			return &guidedStore{fail: invalidKey}, nil
		}
		return original(cfg)
	}
	env.Pause = func(time.Duration) { pauses++ }
	i := fixtureIssuer(t, env, home)
	s, _, err := i.create(issuance.Precreated)
	must(t, err)
	if calls != 3 || pauses != 2 || s.State != issuance.Spare || len(cf.Live()) != 1 {
		t.Fatal("key exposed before propagation checks completed")
	}
}

func TestDedicatedLostResponseAmbiguousNameNeverDeletes(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	client := i.api
	i.api = &trackedInventoryAPI{API: client, InventoryAPI: client.(cloudflare.InventoryAPI), create: func(ctx context.Context, a string, s cloudflare.TokenSpec) (cloudflare.Token, error) {
		_, err := client.CreateToken(ctx, a, s)
		must(t, err)
		cf.MetadataTokens = append(cf.MetadataTokens, map[string]any{"id": strings.Repeat("f", 32), "name": s.Name, "status": "active"})
		return cloudflare.Token{}, errors.New("lost")
	}}
	s, _, err := i.create(issuance.Fresh)
	if err == nil || s.State != issuance.CleanupPending || len(cf.Live()) != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
		t.Fatal("ambiguous provider name authorized cleanup")
	}
}

func TestDedicatedReservedCrashReleaseRequiresNoExposure(t *testing.T) {
	env, home, _, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	s, _, err := i.create(issuance.Precreated)
	must(t, err)
	first, _, err := reserveSpare(home, i.cfg, strings.Repeat("b", 32), "laptop", env.now().Add(time.Hour), env)
	must(t, err)
	must(t, recoverUntouchedSpares(home, i.cfg))
	next, _, err := reserveSpare(home, i.cfg, strings.Repeat("c", 32), "desktop", env.now().Add(time.Hour), env)
	must(t, err)
	if first.SlotID != s.SlotID || next.SlotID != s.SlotID {
		t.Fatal("untouched reservation was lost")
	}
	next.State = issuance.DeliveryIntent
	must(t, issuance.Save(home, next))
	must(t, recoverUntouchedSpares(home, i.cfg))
	final, _, err := reserveSpare(home, i.cfg, strings.Repeat("d", 32), "tablet", env.now().Add(time.Hour), env)
	must(t, err)
	if final.SlotID != "" {
		t.Fatal("uncertain delivery recycled")
	}
}

func TestDedicatedCreateWithIntentBindsRecipientBeforeProvider(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	recipient := strings.Repeat("b", 32)
	called := false
	s, _, err := i.createWithIntent(issuance.Fresh, recipient, func(slot issuance.Slot) error {
		called = true
		slots, err := issuance.List(home)
		must(t, err)
		if len(slots) != 1 || slots[0].SlotID != slot.SlotID || slot.RecipientID != recipient || cf.Calls(cloudflaretest.RouteCreateToken) != 0 {
			t.Fatal("provider preceded transaction intent")
		}
		return nil
	})
	must(t, err)
	id, ok := cloudflare.ParseProviderName(cf.Tokens()[0].Name)
	if !called || !ok || id.RecipientID != recipient || s.RecipientID != recipient {
		t.Fatal("chosen recipient identity changed")
	}
}

func TestDedicatedCreateWithIntentCallbackFailureAndInvalidRecipient(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	s, _, err := i.createWithIntent(issuance.Fresh, strings.Repeat("b", 32), func(issuance.Slot) error { return errors.New("transaction journal failed") })
	if err == nil || s.State != issuance.Deleted || cf.Calls(cloudflaretest.RouteCreateToken) != 0 {
		t.Fatal("callback failure caused API effect")
	}
	slots, err := issuance.List(home)
	must(t, err)
	if len(slots) != 1 || slots[0].CleanupReason != "transaction-refused-before-provider" {
		t.Fatal("callback failure lost durable intent")
	}
	_, _, err = i.createWithIntent(issuance.Fresh, "../invalid", func(issuance.Slot) error { t.Fatal("invalid recipient reached transaction"); return nil })
	if err == nil || cf.Calls(cloudflaretest.RouteCreateToken) != 0 {
		t.Fatal("invalid recipient reached provider")
	}
	slots, err = issuance.List(home)
	must(t, err)
	if len(slots) != 1 {
		t.Fatal("invalid recipient persisted")
	}
}

func TestDedicatedPairingReceiverCommitsSlotAndKeepsLocalIdentity(t *testing.T) {
	source, _, provider, _ := dedicatedFixture(t)
	source.LookupEnv = func(key string) (string, bool) {
		if key == "CLOUDFLARE_API_TOKEN" {
			return bootstrapCanary, true
		}
		return "", false
	}
	var out bytes.Buffer
	if Run([]string{"machines", "add", "--yes", "--name", "laptop", "--spares=0"}, strings.NewReader(""), &out, &out, source) != 0 {
		t.Fatal("source failed")
	}
	bundle, code := pairingPieces(out.String())
	payload, err := pairing.Open(bundle, code, source.now())
	must(t, err)
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	kc := newFakeKeychain()
	env := setupTestEnv(t, home, userHome, kc, source.now())
	env.DetectHarnesses = func(string) []string { return []string{"codex"} }
	env.LookupEnv = func(key string) (string, bool) { return code, key == "AGENT_ARCHIVE_PAIRING_CODE" }
	env.OpenStore = source.OpenStore
	existing := config.Config{MachineID: strings.Repeat("d", 32), Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: payload.Storage.Bucket, Prefix: payload.Storage.Prefix, R2AccountID: payload.Storage.R2Account}}
	must(t, config.Save(home, existing))
	out.Reset()
	if result := Run([]string{"setup", "--pair-file", "-", "--yes", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project}, strings.NewReader(bundle), &out, &out, env); result != 0 {
		t.Fatalf("receiver %d %s", result, &out)
	}
	cfg, _, err := config.Load(home)
	must(t, err)
	if cfg.MachineID != existing.MachineID || cfg.MachineAssignment == nil || cfg.MachineAssignment.Kind != config.MachineAssignmentR2Own || cfg.MachineAssignment.SlotID != payload.SlotID || cfg.MachineAssignment.RecipientID != payload.RecipientID || cfg.MachineAssignment.SharedWith != "" {
		t.Fatal("own-key provenance or local identity lost")
	}
	if !strings.Contains(out.String(), "Dedicated R2 key") || strings.Contains(out.String(), "Shared-key R2") {
		t.Fatal("dedicated receiver mislabeled shared")
	}
	live := len(provider.Live())
	env.Cloudflare = func(string) cloudflare.API {
		t.Fatal("receiving an existing pairing must not issue a provider key")
		return nil
	}
	for range 2 {
		out.Reset()
		if result := Run([]string{"setup", "--pair-file", "-", "--yes", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project}, strings.NewReader(bundle), &out, &out, env); result != 0 {
			t.Fatalf("repeat receiver %d %s", result, &out)
		}
		if len(provider.Live()) != live || len(kc.items) != 1 {
			t.Fatal("repeated receive duplicated provider access or local credentials")
		}
	}
}

func TestDedicatedDistinctBucketClaimsRemainInformational(t *testing.T) {
	env, home, _, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	_, _, err := i.create(issuance.Precreated)
	must(t, err)
	s, _, err := reserveSpare(home, i.cfg, strings.Repeat("b", 32), "laptop", env.now().Add(time.Hour), env)
	must(t, err)
	s.State = issuance.DeliveryIntent
	must(t, issuance.Save(home, s))
	ledger := pairingLedger{Version: 1, SlotID: s.SlotID, PairingID: s.PairingID, RecipientID: s.RecipientID, IssuerID: s.IssuerID, Name: s.Label, DestinationID: s.DestinationID, Kind: pairingOwnR2, AccessKeyID: s.ProviderID, CredentialRef: s.SecretRef, State: pairingDeliveryIntent, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt}
	must(t, savePairingLedger(home, ledger))
	result := machines.ListResult{}
	for _, id := range []string{strings.Repeat("c", 32), strings.Repeat("d", 32)} {
		result.Records = append(result.Records, machines.Record{MachineID: id, PairingID: s.PairingID, PairedFrom: s.IssuerID, Credential: machines.CredentialBinding{Kind: config.MachineAssignmentR2Own, AccessKeyID: s.ProviderID, RecipientID: s.RecipientID, IssuerID: s.IssuerID, SlotID: s.SlotID}})
	}
	observePairingClaims(home, s.DestinationID, result, env.now())
	observePairingClaims(home, s.DestinationID, result, env.now())
	ledgers, err := readPairingLedgers(home)
	must(t, err)
	if len(ledgers[0].ObservedMachineIDs) != 2 || !strings.Contains(strings.Join(pairingWarnings(home, env.now()), " "), "untrusted") {
		t.Fatal("distinct informational claims lost")
	}
	slots, err := issuance.List(home)
	must(t, err)
	if slots[0].State != issuance.DeliveryIntent || slots[0].ProviderID != s.ProviderID {
		t.Fatal("bucket claims mutated authoritative slot")
	}
}

func TestDedicatedOwnIntentNeverEntersSparePool(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	s, _, err := i.createWithIntent(issuance.Fresh, i.cfg.MachineID, func(issuance.Slot) error { return nil })
	must(t, err)
	if s.State != issuance.OwnIntent {
		t.Fatal("own transaction entered spare pool")
	}
	none, _, err := reserveSpare(home, i.cfg, strings.Repeat("b", 32), "laptop", env.now().Add(time.Hour), env)
	must(t, err)
	if none.SlotID != "" {
		t.Fatal("own staged key handed out")
	}
	must(t, i.reconcile())
	i.cleanup(&s)
	if s.State != issuance.OwnIntent || len(cf.Live()) != 1 {
		t.Fatal("ordinary source cleanup touched own transaction")
	}
}
