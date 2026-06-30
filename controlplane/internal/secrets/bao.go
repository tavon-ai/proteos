package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	bao "github.com/openbao/openbao/api/v2"
)

// BaoStore is the production secrets backend: an OpenBao KV v2 mount behind the
// same Store interface as FileStore (decision #1). The interface's canonical
// "secret/users/..." / "secret/machines/..." paths map onto KV v2's data and
// metadata sub-paths inside this client, so every existing caller moves to
// OpenBao by config alone, with zero call-site changes.
//
// Per-user enforcement lives in OpenBao, not just in Go (decision #2): the
// control-plane (base) token cannot read any user secret. Every operation on a
// secret/users/<uid>/... path is performed with a short-lived orphan child
// token scoped by policy user-<uid> to that user's subtree, minted via the
// proteos-user token role. A confused-deputy bug that builds user B's path
// while acting for user A therefore fails in Bao, not merely in our code.
// Machine paths (secret/machines/...) are covered directly by the base token's
// cp-base policy and use the base client.
type BaoStore struct {
	mount  string // KV v2 mount, default "secret"
	prefix string // optional path namespace inside the mount, e.g. "proteos/" (always "" or trailing-slash terminated)

	mu       sync.Mutex
	client   *bao.Client
	roleID   string // AppRole role_id; empty when constructed with a static token
	secretID string

	ensured map[string]struct{} // user ids whose policy has been ensured this process
}

// childTokenTTL is the lifetime of a per-user child token. It only needs to
// outlive a single Put/Get/Delete, so it is deliberately short (decision #2:
// 90s). Renewable is disabled by the token role.
const childTokenTTL = "90s"

// BaoConfig configures a BaoStore. Exactly one auth method must be supplied:
// either a static Token (tests / migration tooling) or an AppRole RoleID plus
// a SecretIDFile (production, per decision #3).
type BaoConfig struct {
	Address      string // OpenBao API address, e.g. http://openbao:8200
	Mount        string // KV v2 mount; defaults to "secret"
	Prefix       string // optional path namespace inside the mount (e.g. "proteos"); leading/trailing slashes are normalized away
	Token        string // static token; takes precedence over AppRole when set
	RoleID       string // AppRole role_id
	SecretIDFile string // path to a file holding the AppRole secret_id
}

// NewBaoStore connects to OpenBao and authenticates per cfg. With AppRole it
// logs in immediately; the resulting base token is re-acquired automatically on
// permission failure (relogin), so a control plane outliving the token TTL keeps
// working.
func NewBaoStore(cfg BaoConfig) (*BaoStore, error) {
	mount := cfg.Mount
	if mount == "" {
		mount = "secret"
	}
	apiCfg := bao.DefaultConfig()
	if cfg.Address != "" {
		apiCfg.Address = cfg.Address
	}
	client, err := bao.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("openbao client: %w", err)
	}
	s := &BaoStore{
		mount:   mount,
		prefix:  normalizePrefix(cfg.Prefix),
		client:  client,
		roleID:  cfg.RoleID,
		ensured: make(map[string]struct{}),
	}
	if cfg.SecretIDFile != "" {
		b, err := os.ReadFile(cfg.SecretIDFile)
		if err != nil {
			return nil, fmt.Errorf("read secret_id file: %w", err)
		}
		s.secretID = strings.TrimSpace(string(b))
	}
	switch {
	case cfg.Token != "":
		client.SetToken(cfg.Token)
	case cfg.RoleID != "":
		if err := s.appRoleLogin(); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("openbao: no auth configured (need Token or RoleID+SecretIDFile)")
	}
	return s, nil
}

// appRoleLogin exchanges the AppRole role_id/secret_id for a fresh base token
// and installs it on the client. Caller holds no lock; it takes s.mu.
func (s *BaoStore) appRoleLogin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appRoleLoginLocked()
}

func (s *BaoStore) appRoleLoginLocked() error {
	sec, err := s.client.Logical().WriteWithContext(context.Background(), "auth/approle/login", map[string]any{
		"role_id":   s.roleID,
		"secret_id": s.secretID,
	})
	if err != nil {
		return fmt.Errorf("approle login: %w", err)
	}
	if sec == nil || sec.Auth == nil || sec.Auth.ClientToken == "" {
		return errors.New("approle login: no token in response")
	}
	s.client.SetToken(sec.Auth.ClientToken)
	return nil
}

// Put writes data at path, overwriting any existing version.
func (s *BaoStore) Put(path string, data map[string]string) error {
	rel, uid, isUser, err := splitPath(path)
	if err != nil {
		return err
	}
	payload := make(map[string]any, len(data))
	for k, v := range data {
		payload[k] = v
	}
	return s.do(uid, isUser, func(c *bao.Client) error {
		_, err := c.Logical().WriteWithContext(context.Background(), s.dataPath(rel),
			map[string]any{"data": payload})
		return err
	})
}

// Get reads the data map at path, or ErrNotFound.
func (s *BaoStore) Get(path string) (map[string]string, error) {
	rel, uid, isUser, err := splitPath(path)
	if err != nil {
		return nil, err
	}
	var out map[string]string
	err = s.do(uid, isUser, func(c *bao.Client) error {
		sec, err := c.Logical().ReadWithContext(context.Background(), s.dataPath(rel))
		if err != nil {
			return err
		}
		if sec == nil || sec.Data == nil {
			return ErrNotFound
		}
		raw, ok := sec.Data["data"].(map[string]any)
		if !ok || len(raw) == 0 {
			return ErrNotFound
		}
		out = make(map[string]string, len(raw))
		for k, v := range raw {
			out[k] = fmt.Sprint(v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes the secret at path (all versions). Deleting a missing path is
// not an error, matching FileStore.
func (s *BaoStore) Delete(path string) error {
	rel, uid, isUser, err := splitPath(path)
	if err != nil {
		return err
	}
	return s.do(uid, isUser, func(c *bao.Client) error {
		// Destroy every version by deleting the metadata, so a later Get is a
		// clean ErrNotFound rather than a soft-deleted tombstone.
		_, err := c.Logical().DeleteWithContext(context.Background(), s.metadataPath(rel))
		return err
	})
}

// do runs fn with the correct client for the path: a per-user child token for
// user paths, the base token otherwise. Base-token operations relogin once on a
// permission error (AppRole token TTL expiry); user-token failures are returned
// as-is — a 403 there is a genuine authorization result (e.g. the denial test).
func (s *BaoStore) do(uid string, isUser bool, fn func(*bao.Client) error) error {
	if isUser {
		c, err := s.userClient(uid)
		if err != nil {
			return err
		}
		return fn(c)
	}
	err := fn(s.baseClient())
	if err != nil && s.roleID != "" && isPermissionError(err) {
		if relErr := s.appRoleLogin(); relErr == nil {
			return fn(s.baseClient())
		}
	}
	return err
}

func (s *BaoStore) baseClient() *bao.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// userClient ensures policy user-<uid> exists and returns a clone of the base
// client carrying a fresh, short-lived child token scoped to that policy.
func (s *BaoStore) userClient(uid string) (*bao.Client, error) {
	if err := s.ensurePolicy(uid); err != nil {
		return nil, err
	}
	tok, err := s.mintUserToken(uid)
	if err != nil {
		return nil, err
	}
	c, err := s.baseClient().Clone()
	if err != nil {
		return nil, fmt.Errorf("clone client: %w", err)
	}
	c.SetToken(tok)
	return c, nil
}

// ensurePolicy idempotently writes policy user-<uid>, scoped to that user's KV
// v2 data + metadata subtree. The in-process cache avoids re-writing the policy
// on every operation; OpenBao itself is the source of truth, so a cache miss
// after a restart simply re-writes an identical policy.
func (s *BaoStore) ensurePolicy(uid string) error {
	s.mu.Lock()
	if _, ok := s.ensured[uid]; ok {
		s.mu.Unlock()
		return nil
	}
	client := s.client
	s.mu.Unlock()

	rules := fmt.Sprintf(`
path "%[1]s/data/%[3]susers/%[2]s/*" {
  capabilities = ["create", "update", "read", "delete"]
}
path "%[1]s/metadata/%[3]susers/%[2]s/*" {
  capabilities = ["read", "delete", "list"]
}
`, s.mount, uid, s.prefix)

	if err := client.Sys().PutPolicyWithContext(context.Background(), userPolicyName(uid), rules); err != nil {
		if s.roleID != "" && isPermissionError(err) {
			if relErr := s.appRoleLogin(); relErr == nil {
				err = s.baseClient().Sys().PutPolicyWithContext(context.Background(), userPolicyName(uid), rules)
			}
		}
		if err != nil {
			return fmt.Errorf("ensure policy %s: %w", userPolicyName(uid), err)
		}
	}

	s.mu.Lock()
	s.ensured[uid] = struct{}{}
	s.mu.Unlock()
	return nil
}

// mintUserToken creates an orphan child token bound to policy user-<uid> via the
// proteos-user token role and returns its client token.
func (s *BaoStore) mintUserToken(uid string) (string, error) {
	create := func() (*bao.Secret, error) {
		return s.baseClient().Logical().WriteWithContext(context.Background(),
			"auth/token/create/"+userTokenRole, map[string]any{
				"policies": []string{userPolicyName(uid)},
				"ttl":      childTokenTTL,
			})
	}
	sec, err := create()
	if err != nil && s.roleID != "" && isPermissionError(err) {
		if relErr := s.appRoleLogin(); relErr == nil {
			sec, err = create()
		}
	}
	if err != nil {
		return "", fmt.Errorf("mint user token: %w", err)
	}
	if sec == nil || sec.Auth == nil || sec.Auth.ClientToken == "" {
		return "", errors.New("mint user token: no token in response")
	}
	return sec.Auth.ClientToken, nil
}

// userTokenRole is the OpenBao token role (created by openbao-init.sh) whose
// allowed_policies_glob is ["user-*"], orphan, ttl=90s, renewable=false.
const userTokenRole = "proteos-user"

func userPolicyName(uid string) string { return "user-" + uid }

func (s *BaoStore) dataPath(rel string) string     { return s.mount + "/data/" + s.prefix + rel }
func (s *BaoStore) metadataPath(rel string) string { return s.mount + "/metadata/" + s.prefix + rel }

// normalizePrefix strips any leading/trailing slashes from a configured path
// namespace and re-adds a single trailing slash, so callers can concatenate it
// directly. An empty (or slash-only) prefix normalizes to "" (no namespace).
func normalizePrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// splitPath maps a canonical interface path (always "secret/<rest>") to its KV
// v2-relative remainder, and classifies it as a user path (returning the user
// id) or not. The literal leading "secret/" is the path-convention namespace
// marker, stripped here and replaced by the configured mount in dataPath.
func splitPath(path string) (rel, uid string, isUser bool, err error) {
	rest, ok := strings.CutPrefix(path, "secret/")
	if !ok {
		return "", "", false, fmt.Errorf("secrets: path %q missing secret/ prefix", path)
	}
	if rest == "" {
		return "", "", false, fmt.Errorf("secrets: empty path %q", path)
	}
	segs := strings.Split(rest, "/")
	if segs[0] == "users" {
		if len(segs) < 3 || segs[1] == "" {
			return "", "", false, fmt.Errorf("secrets: malformed user path %q", path)
		}
		return rest, segs[1], true, nil
	}
	return rest, "", false, nil
}

// isPermissionError reports whether err is an OpenBao 403 (permission denied),
// the signal to relogin a stale base token.
func isPermissionError(err error) bool {
	var re *bao.ResponseError
	if errors.As(err, &re) {
		return re.StatusCode == 403
	}
	return false
}

// IsPermissionDenied reports whether err (possibly wrapped) is an OpenBao 403.
// Callers use it to distinguish a misconfigured policy — which will not
// self-heal and should fail the process — from a transient/connectivity error.
func IsPermissionDenied(err error) bool { return isPermissionError(err) }

// compile-time check
var _ Store = (*BaoStore)(nil)
