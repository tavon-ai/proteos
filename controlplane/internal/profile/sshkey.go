package profile

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// GenerateSSHKey mints a fresh ed25519 keypair for the portable profile and
// returns the OpenSSH-format private key (PEM), the authorized_keys-format public
// key (with a "proteos" comment, for the user to add to GitHub), and the public
// key's SHA256 fingerprint. The private key is generated server-side and stored
// in OpenBao; it is never returned to the browser.
func GenerateSSHKey() (privatePEM, publicKey, fingerprint string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", "", fmt.Errorf("generate ed25519 key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", "", "", fmt.Errorf("marshal private key: %w", err)
	}
	privatePEM = string(pem.EncodeToMemory(block))

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", "", fmt.Errorf("derive public key: %w", err)
	}
	// MarshalAuthorizedKey returns "ssh-ed25519 AAAA...\n"; add a comment so the
	// key is recognizable when pasted into GitHub.
	authorized := strings.TrimRight(string(ssh.MarshalAuthorizedKey(sshPub)), "\n")
	publicKey = authorized + " proteos\n"
	fingerprint = ssh.FingerprintSHA256(sshPub)
	return privatePEM, publicKey, fingerprint, nil
}

// SSHFingerprint parses an authorized_keys-format public key and returns its
// SHA256 fingerprint, for display alongside a stored key.
func SSHFingerprint(publicKey string) (string, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return "", fmt.Errorf("parse public key: %w", err)
	}
	return ssh.FingerprintSHA256(pk), nil
}
