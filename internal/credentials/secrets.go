package credentials

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/amalychev/devopus/internal/agent/paths"
)

// Where the refresh token is kept. The system key store first; a file readable only
// by the user where there is none.
const (
	StoreKeychain      = "keychain"       // macOS
	StoreSecretService = "secret-service" // Linux: GNOME Keyring, KWallet
	StoreFile          = "file"
)

// service names the item in the key store; the account is the data directory, so a
// second instance (DEVOPUS_HOME) signs in on its own.
const service = "devopus"

var errNoSecret = errors.New("no refresh token in the key store")

type secrets interface {
	get(account string) (string, error)
	set(account, secret string) error
	del(account string) error
}

// pickStore chooses the key store: $DEVOPUS_CREDENTIAL_STORE, or the system's.
func pickStore() string {
	if s := os.Getenv("DEVOPUS_CREDENTIAL_STORE"); s != "" {
		return s
	}
	switch goruntime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("security"); err == nil {
			return StoreKeychain
		}
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err == nil && os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
			return StoreSecretService
		}
	}
	return StoreFile
}

func open(store string) (secrets, error) {
	switch store {
	case StoreKeychain:
		return keychain{}, nil
	case StoreSecretService:
		return secretService{}, nil
	case StoreFile:
		return file{}, nil
	}
	return nil, fmt.Errorf("unknown credential store %q", store)
}

// keychain keeps the token in the login keychain through security(1). The token is
// passed on stdin of an interactive session, not in arguments other processes see.
type keychain struct{}

func (keychain) get(account string) (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 { // errSecItemNotFound
			return "", errNoSecret
		}
		return "", fmt.Errorf("keychain: %v", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (k keychain) set(account, secret string) error {
	if strings.ContainsAny(account+secret, "\"\\\n") {
		return fmt.Errorf("keychain: unsupported characters in %q", account)
	}
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a \"%s\" -l \"Devopus sign-in\" -w \"%s\"\n", service, account, secret))
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("keychain: %v: %s", err, strings.TrimSpace(errOut.String()))
	}
	// security -i reports a failed command on stderr, not in its exit code.
	if got, err := k.get(account); err != nil || got != secret {
		return fmt.Errorf("keychain: the token was not saved: %s", strings.TrimSpace(errOut.String()))
	}
	return nil
}

func (keychain) del(account string) error {
	err := exec.Command("security", "delete-generic-password", "-s", service, "-a", account).Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 44 {
		return nil
	}
	return err
}

// secretService keeps the token through secret-tool(1), which reads it from stdin.
type secretService struct{}

func (secretService) get(account string) (string, error) {
	out, err := exec.Command("secret-tool", "lookup", "service", service, "account", account).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(out) == 0 {
			return "", errNoSecret
		}
		return "", fmt.Errorf("secret service: %v", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (secretService) set(account, secret string) error {
	cmd := exec.Command("secret-tool", "store", "--label=Devopus sign-in", "service", service, "account", account)
	cmd.Stdin = strings.NewReader(secret)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("secret service: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (secretService) del(account string) error {
	return exec.Command("secret-tool", "clear", "service", service, "account", account).Run()
}

// file keeps the token in a file only the user can read: where no key store exists.
type file struct{}

func tokenFile() string { return filepath.Join(paths.Home(), "refresh-token") }

func (file) get(string) (string, error) {
	b, err := os.ReadFile(tokenFile())
	if errors.Is(err, os.ErrNotExist) {
		return "", errNoSecret
	}
	return strings.TrimSpace(string(b)), err
}

func (file) set(_, secret string) error {
	return writeFile(tokenFile(), []byte(secret+"\n"))
}

func (file) del(string) error {
	if err := os.Remove(tokenFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// writeFile replaces a file atomically, readable only by the user.
func writeFile(path string, data []byte) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
