// Package sshx runs commands on tenant nodes over SSH with pinned host keys.
package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// KeyPair is an ed25519 key pair in the formats we need.
type KeyPair struct {
	Signer        ssh.Signer
	AuthorizedKey string // "ssh-ed25519 AAAA..." without trailing newline
	PrivatePEM    []byte // OpenSSH private key format
}

// NewKeyPair generates an ed25519 key pair.
func NewKeyPair(comment string) (KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return KeyPair{}, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return KeyPair{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{
		Signer:        signer,
		AuthorizedKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))),
		PrivatePEM:    pem.EncodeToMemory(block),
	}, nil
}

// ParseKeyPair loads a key pair from an OpenSSH private key.
func ParseKeyPair(privatePEM []byte) (KeyPair, error) {
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{
		Signer:        signer,
		AuthorizedKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
		PrivatePEM:    privatePEM,
	}, nil
}

// Target is a node we can run commands on.
type Target struct {
	Addr    string // host:port
	User    string
	Signer  ssh.Signer
	HostKey string // authorized_keys format, pinned
}

func (t Target) config() (*ssh.ClientConfig, error) {
	hk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.HostKey))
	if err != nil {
		return nil, fmt.Errorf("parse pinned host key: %w", err)
	}
	return &ssh.ClientConfig{
		User:            t.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(t.Signer)},
		HostKeyCallback: ssh.FixedHostKey(hk),
		Timeout:         10 * time.Second,
	}, nil
}

// ErrExit wraps a non-zero remote exit status.
type ErrExit struct {
	Status int
	Stderr string
}

func (e *ErrExit) Error() string {
	s := strings.TrimSpace(e.Stderr)
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return fmt.Sprintf("remote command exited %d: %s", e.Status, s)
}

// Run executes cmd and returns stdout. stdin may be nil. Secrets must only be
// passed through stdin, never in cmd, because cmd can end up in logs.
func (t Target) Run(ctx context.Context, cmd string, stdin io.Reader) ([]byte, error) {
	cfg, err := t.config()
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, t.Addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if stdin != nil {
		sess.Stdin = stdin
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		client.Close()
		return stdout.Bytes(), ctx.Err()
	case err := <-done:
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return stdout.Bytes(), &ErrExit{Status: ee.ExitStatus(), Stderr: stderr.String()}
		}
		return stdout.Bytes(), err
	}
}

// WaitReady polls until a trivial command succeeds or ctx expires.
func (t Target) WaitReady(ctx context.Context) error {
	var last error
	for {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := t.Run(cctx, "true", nil)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("ssh %s not ready: %w (last error: %v)", t.Addr, ctx.Err(), last)
		case <-time.After(5 * time.Second):
		}
	}
}
