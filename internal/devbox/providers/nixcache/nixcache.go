package nixcache

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"go.jetpack.io/devbox/internal/build"
	"go.jetpack.io/devbox/internal/debug"
	"go.jetpack.io/devbox/internal/devbox/providers/identity"
	"go.jetpack.io/devbox/internal/envir"
	"go.jetpack.io/devbox/internal/fileutil"
	"go.jetpack.io/devbox/internal/nix"
	"go.jetpack.io/devbox/internal/redact"
	"go.jetpack.io/devbox/internal/ux"
	"go.jetpack.io/pkg/api"
	nixv1alpha1 "go.jetpack.io/pkg/api/gen/priv/nix/v1alpha1"
	"go.jetpack.io/pkg/filecache"
)

type Provider struct{}

var singleton *Provider = &Provider{}

func Get() *Provider {
	return singleton
}

func (p *Provider) Configure(ctx context.Context, username string) error {
	debug.Log("checking if nix cache is configured for %s", username)

	rootConfig, err := p.rootAWSConfigPath()
	if err != nil {
		return err
	}
	debug.Log("root aws config path is: %s", rootConfig)
	awsConfigExists := fileutil.Exists(rootConfig)

	cfg, err := nix.CurrentConfig(ctx)
	if err != nil {
		return err
	}
	trusted, _ := cfg.IsUserTrusted(ctx, username)

	configured := awsConfigExists && trusted
	debug.Log("nix cache configured = %v (awsConfigExists == %v && trusted == %v)", configured, awsConfigExists, trusted)
	if configured {
		return nil
	}

	if os.Getuid() == 0 {
		err := p.configureRoot(ctx, username)
		if err != nil {
			return redact.Errorf("update ~root/.aws/config with devbox credentials: %s", err)
		}
		return nil
	}

	_, err = nix.DaemonVersion(ctx)
	if err == nil {
		// It looks like this is a multi-user install running a Nix
		// daemon, so we need to configure AWS S3 authentication for the
		// root user.
		if err := p.sudoConfigureRoot(ctx, username); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) rootAWSConfigPath() (string, error) {
	u, err := user.LookupId("0")
	if err != nil {
		return "", redact.Errorf("lookup root user: %s", err)
	}
	if u.HomeDir == "" {
		return "", redact.Errorf("empty root user home directory: %s", u.Username, err)
	}
	return filepath.Join(u.HomeDir, ".aws", "config"), nil
}

func (p *Provider) configureRoot(ctx context.Context, username string) error {
	exe := p.executable()
	if exe == "" {
		return redact.Errorf("get path to current devbox executable")
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return redact.Errorf("get path to sudo executable: %s", err)
	}
	path, err := p.rootAWSConfigPath()
	if err != nil {
		return err
	}

	// Rename the .aws directory in case it already exists. We should
	// improve this to be more careful with existing ~root/.aws/configs, but
	// this seems rare enough that it should be okay for the initial
	// implementation.
	dir := filepath.Dir(path)
	_ = os.Rename(dir, dir+".bak") // ignore errors for non-existent dir
	_ = os.Mkdir(dir, 0o755)       // ignore errors for dir exists (don't os.MkdirAll the home directory)

	config, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.FileMode(0o644))
	if err != nil {
		return err
	}
	defer config.Close()

	// TODO(gcurtis): it would be nice to use a non-default profile if
	// https://github.com/NixOS/nix/issues/5525 ever gets fixed.
	_, err = fmt.Fprintf(config, `# This file was generated by Devbox.
# Any overwritten configs can be found in the .aws.bak directory.

[default]
# sudo as the configured user so that their cached credential files have the
# correct ownership.
credential_process = %s -u %s -i %s cache credentials
`, sudo, username, exe)
	if err != nil {
		return err
	}
	if err := config.Close(); err != nil {
		return err
	}

	if err := nix.IncludeDevboxConfig(ctx, username); err != nil {
		return redact.Errorf("modify nix config: %v", err)
	}
	return nil
}

func (p *Provider) sudoConfigureRoot(ctx context.Context, username string) error {
	// TODO(gcurtis): save the user's response so that we don't pester them
	// every time if it's a no.
	prompt := &survey.Confirm{
		Message: "Devbox requires root to configure the Nix daemon to use your organization's private cache. Allow sudo?",
	}
	ok := false
	if err := survey.AskOne(prompt, &ok); err != nil {
		return err
	}
	if !ok {
		return nil
	}

	exe := p.executable()
	if exe == "" {
		return redact.Errorf("get path to current devbox executable")
	}

	cmd := exec.CommandContext(ctx, "sudo", exe, "cache", "configure", "--user", username)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	debug.Log("running sudo: %s", cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to relaunch with sudo: %w", err)
	}

	// Print a warning if we were unable to automatically make the user
	// trusted.
	checkIfUserCanAddSubstituter(ctx)
	return nil
}

func (*Provider) executable() string {
	if exe := os.Getenv(envir.LauncherPath); exe != "" {
		return exe
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return ""
}

// Credentials fetches short-lived credentials that grant access to the user's
// private cache.
func (p *Provider) Credentials(ctx context.Context) (AWSCredentials, error) {
	cache := filecache.New[AWSCredentials]("devbox/providers/nixcache")
	token, err := identity.Get().GenSession(ctx)
	if err != nil {
		return AWSCredentials{}, err
	}
	creds, err := cache.GetOrSetWithTime(
		"credentials-"+token.IDClaims().Subject,
		func() (AWSCredentials, time.Time, error) {
			token, err := identity.Get().GenSession(ctx)
			if err != nil {
				return AWSCredentials{}, time.Time{}, err
			}
			client := api.NewClient(ctx, build.JetpackAPIHost(), token)
			creds, err := client.GetAWSCredentials(ctx)
			if err != nil {
				return AWSCredentials{}, time.Time{}, err
			}
			exp := time.Time{}
			if t := creds.GetExpiration(); t != nil {
				exp = t.AsTime()
			}
			return newAWSCredentials(creds), exp, nil
		},
	)
	if err != nil {
		return AWSCredentials{}, redact.Errorf("nixcache: get credentials: %w", redact.Safe(err))
	}
	return creds, nil
}

// URI queries the Jetify API for the URI that points to user's private cache.
// If their account doesn't have access to a cache, it returns an empty string
// and a nil error.
func (p *Provider) URI(ctx context.Context) (string, error) {
	cache := filecache.New[string]("devbox/providers/nixcache")
	token, err := identity.Get().GenSession(ctx)
	if err != nil {
		return "", err
	}
	// Landau: I think we can probably remove this cache? This endpoint is very
	// fast and we only use this for build/upload which are slow.
	uri, err := cache.GetOrSet(
		"uri-"+token.IDClaims().Subject,
		func() (string, time.Duration, error) {
			client := api.NewClient(ctx, build.JetpackAPIHost(), token)
			resp, err := client.GetBinCache(ctx)
			if err != nil {
				return "", 0, redact.Errorf("nixcache: get uri: %w", redact.Safe(err))
			}

			// Don't cache negative responses.
			if resp.GetNixBinCacheUri() == "" {
				return "", 0, nil
			}

			// TODO(gcurtis): do a better job of invalidating the URI after
			// a Nix command fails to query the cache.
			return resp.GetNixBinCacheUri(), 24 * time.Hour, nil
		},
	)
	if err != nil {
		return "", redact.Errorf("nixcache: get uri: %w", redact.Safe(err))
	}
	return uri, nil
}

func checkIfUserCanAddSubstituter(ctx context.Context) {
	// we need to ensure that the user can actually use the extra
	// substituter. If the user did a root install, then we need to add
	// the trusted user/substituter to the nix.conf file and restart the daemon.

	// This check is not perfect, so we still try to use the substituter even if
	// it fails

	// TODOs:
	// * Also check if cache is enabled in nix.conf
	// * Test on single user install
	// * Automate making user trusted if needed
	cfg, err := nix.CurrentConfig(ctx)
	if err != nil {
		return
	}

	u, err := user.Current()
	if err != nil {
		return
	}
	trusted, _ := cfg.IsUserTrusted(ctx, u.Username)
	if !trusted {
		ux.Fwarning(
			os.Stderr,
			"In order to use a custom nix cache you must be a trusted user. Please "+
				"add your username to nix.conf (usually located at /etc/nix/nix.conf)"+
				" and restart the nix daemon.\n",
		)
	}
}

// AWSCredentials are short-lived credentials that grant access to a private Nix
// cache in S3. It marshals to JSON per the schema described in
// `aws help config-vars` under "Sourcing Credentials From External Processes".
type AWSCredentials struct {
	// Version must always be 1.
	Version         int       `json:"Version"`
	AccessKeyID     string    `json:"AccessKeyId"`
	SecretAccessKey string    `json:"SecretAccessKey"`
	SessionToken    string    `json:"SessionToken"`
	Expiration      time.Time `json:"Expiration"`
}

func newAWSCredentials(proto *nixv1alpha1.AWSCredentials) AWSCredentials {
	creds := AWSCredentials{
		Version:         1,
		AccessKeyID:     proto.AccessKeyId,
		SecretAccessKey: proto.SecretKey,
		SessionToken:    proto.SessionToken,
	}
	if proto.Expiration != nil {
		creds.Expiration = proto.Expiration.AsTime()
	}
	return creds
}

// Env returns the credentials as a slice of environment variables.
func (a AWSCredentials) Env() []string {
	return []string{
		"AWS_ACCESS_KEY_ID=" + a.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + a.SecretAccessKey,
		"AWS_SESSION_TOKEN=" + a.SessionToken,
	}
}
