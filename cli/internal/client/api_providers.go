package client

import "context"

// SecretField is one declared input a provider needs, as returned by GET
// /api/providers. Name/Label/Env are not secret — only the value the caller
// supplies (via SetProviderKey) is.
type SecretField struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Env   string `json:"env"`
}

// Provider is one row of GET /api/providers: the registry metadata plus
// whether the caller has a key stored for it. The key material itself is
// never part of this (or any) response — KeySet is the only signal.
type Provider struct {
	Key          string        `json:"key"`
	DisplayName  string        `json:"display_name"`
	Enabled      bool          `json:"enabled"`
	KeySet       bool          `json:"key_set"`
	SecretFields []SecretField `json:"secret_fields"`
}

// ListProviders returns the provider catalog plus the caller's key_set status
// per provider.
func (c *Client) ListProviders(ctx context.Context) ([]Provider, error) {
	var ps []Provider
	err := c.Do(ctx, "GET", "/api/providers", nil, &ps)
	return ps, err
}

// setProviderKeyRequest is the PUT /api/secrets/providers/{key} body: one
// value per field the provider declares (Provider.SecretFields).
type setProviderKeyRequest struct {
	Fields map[string]string `json:"fields"`
}

// SetProviderKey stores (or replaces) the caller's secret fields for the
// provider identified by key. Values are write-only — the server responds 204
// and never echoes them back.
func (c *Client) SetProviderKey(ctx context.Context, key string, fields map[string]string) error {
	return c.Do(ctx, "PUT", "/api/secrets/providers/"+key, setProviderKeyRequest{Fields: fields}, nil)
}

// DeleteProviderKey removes the caller's stored secret for the provider
// identified by key.
func (c *Client) DeleteProviderKey(ctx context.Context, key string) error {
	return c.Do(ctx, "DELETE", "/api/secrets/providers/"+key, nil, nil)
}
