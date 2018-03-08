package sub

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/justwatchcom/gopass/backend"
	gpgcli "github.com/justwatchcom/gopass/backend/crypto/gpg/cli"
	gpgmock "github.com/justwatchcom/gopass/backend/crypto/gpg/mock"
	"github.com/justwatchcom/gopass/backend/crypto/gpg/openpgp"
	"github.com/justwatchcom/gopass/backend/crypto/xc"
	"github.com/justwatchcom/gopass/backend/store/fs"
	kvconsul "github.com/justwatchcom/gopass/backend/store/kv/consul"
	kvmock "github.com/justwatchcom/gopass/backend/store/kv/mock"
	gitcli "github.com/justwatchcom/gopass/backend/sync/git/cli"
	"github.com/justwatchcom/gopass/backend/sync/git/gogit"
	gitmock "github.com/justwatchcom/gopass/backend/sync/git/mock"
	"github.com/justwatchcom/gopass/store"
	"github.com/justwatchcom/gopass/utils/agent/client"
	"github.com/justwatchcom/gopass/utils/ctxutil"
	"github.com/justwatchcom/gopass/utils/fsutil"
	"github.com/justwatchcom/gopass/utils/out"
	"github.com/muesli/goprogressbar"
	"github.com/pkg/errors"
)

// Store is password store
type Store struct {
	alias  string
	url    *backend.URL
	crypto backend.Crypto
	sync   backend.Sync
	store  backend.Store
	cfgdir string
}

// New creates a new store, copying settings from the given root store
func New(ctx context.Context, alias, path string, cfgdir string) (*Store, error) {
	u, err := backend.ParseURL(path)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse path URL '%s': %s", path, err)
	}

	s := &Store{
		alias:  alias,
		url:    u,
		sync:   gitmock.New(),
		cfgdir: cfgdir,
	}

	// init store backend
	if backend.HasStoreBackend(ctx) {
		s.url.Store = backend.GetStoreBackend(ctx)
	}
	if err := s.initStoreBackend(ctx); err != nil {
		return nil, err
	}

	// init sync backend
	if backend.HasSyncBackend(ctx) {
		s.url.Sync = backend.GetSyncBackend(ctx)
	}
	if err := s.initSyncBackend(ctx); err != nil {
		return nil, err
	}

	// init crypto backend
	if backend.HasCryptoBackend(ctx) {
		s.url.Crypto = backend.GetCryptoBackend(ctx)
	}
	if err := s.initCryptoBackend(ctx); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Store) initStoreBackend(ctx context.Context) error {
	switch s.url.Store {
	case backend.FS:
		out.Debug(ctx, "Using Store Backend: fs")
		s.store = fs.New(s.url.Path)
	case backend.KVMock:
		out.Debug(ctx, "Using Store Backend: kvmock")
		s.store = kvmock.New()
	case backend.Consul:
		out.Debug(ctx, "Using Store Backend: consul")
		store, err := kvconsul.New(s.url.Host+":"+s.url.Port, s.url.Query.Get("datacenter"), s.url.Query.Get("token"))
		if err != nil {
			return err
		}
		s.store = store
	default:
		return fmt.Errorf("Unknown store backend")
	}
	return nil
}

func (s *Store) initSyncBackend(ctx context.Context) error {
	switch s.url.Sync {
	case backend.GoGit:
		out.Cyan(ctx, "WARNING: Using experimental sync backend 'go-git'")
		git, err := gogit.Open(s.url.Path)
		if err != nil {
			out.Debug(ctx, "Failed to initialize sync backend 'gogit': %s", err)
		} else {
			s.sync = git
			out.Debug(ctx, "Using Sync Backend: go-git")
		}
	case backend.GitCLI:
		gpgBin, _ := gpgcli.Binary(ctx, "")
		git, err := gitcli.Open(s.url.Path, gpgBin)
		if err != nil {
			out.Debug(ctx, "Failed to initialize sync backend 'gitcli': %s", err)
		} else {
			s.sync = git
			out.Debug(ctx, "Using Sync Backend: gitcli")
		}
	case backend.GitMock:
		// no-op
		out.Debug(ctx, "Using Sync Backend: git-mock")
	default:
		return fmt.Errorf("Unknown Sync Backend")
	}
	return nil
}

func (s *Store) initCryptoBackend(ctx context.Context) error {
	switch s.url.Crypto {
	case backend.GPGCLI:
		out.Debug(ctx, "Using Crypto Backend: gpg-cli")
		gpg, err := gpgcli.New(ctx, gpgcli.Config{
			Umask: fsutil.Umask(),
			Args:  gpgcli.GPGOpts(),
		})
		if err != nil {
			return err
		}
		s.crypto = gpg
	case backend.XC:
		out.Debug(ctx, "Using Crypto Backend: xc (EXPERIMENTAL)")
		crypto, err := xc.New(s.cfgdir, client.New(s.cfgdir))
		if err != nil {
			return err
		}
		s.crypto = crypto
	case backend.GPGMock:
		out.Debug(ctx, "Using Crypto Backend: gpg-mock (NO ENCRYPTION)")
		s.crypto = gpgmock.New()
	case backend.OpenPGP:
		out.Debug(ctx, "Using Crypto Backend: openpgp (ALPHA)")
		crypto, err := openpgp.New(ctx)
		if err != nil {
			return err
		}
		s.crypto = crypto
	default:
		return fmt.Errorf("no valid crypto backend selected")
	}
	return nil
}

// idFile returns the path to the recipient list for this store
// it walks up from the given filename until it finds a directory containing
// a gpg id file or it leaves the scope of this store.
func (s *Store) idFile(ctx context.Context, name string) string {
	fn := name
	var cnt uint8
	for {
		cnt++
		if cnt > 100 {
			break
		}
		if fn == "" || fn == sep {
			break
		}
		gfn := filepath.Join(fn, s.crypto.IDFile())
		if s.store.Exists(ctx, gfn) {
			return gfn
		}
		fn = filepath.Dir(fn)
	}
	return s.crypto.IDFile()
}

// Equals returns true if this store has the same on-disk path as the other
func (s *Store) Equals(other *Store) bool {
	if other == nil {
		return false
	}
	return s.url.String() == other.url.String()
}

// IsDir returns true if the entry is folder inside the store
func (s *Store) IsDir(ctx context.Context, name string) bool {
	return s.store.IsDir(ctx, name)
}

// Exists checks the existence of a single entry
func (s *Store) Exists(ctx context.Context, name string) bool {
	return s.store.Exists(ctx, s.passfile(name))
}

func (s *Store) useableKeys(ctx context.Context, name string) ([]string, error) {
	rs, err := s.GetRecipients(ctx, name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get recipients")
	}

	if !IsCheckRecipients(ctx) {
		return rs, nil
	}

	kl, err := s.crypto.FindPublicKeys(ctx, rs...)
	if err != nil {
		return rs, err
	}

	return kl, nil
}

// passfile returns the name of gpg file on disk, for the given key/name
func (s *Store) passfile(name string) string {
	return strings.TrimPrefix(name+"."+s.crypto.Ext(), "/")
}

// String implement fmt.Stringer
func (s *Store) String() string {
	return fmt.Sprintf("Store(Alias: %s, Path: %s)", s.alias, s.url.String())
}

// reencrypt will re-encrypt all entries for the current recipients
func (s *Store) reencrypt(ctx context.Context) error {
	entries, err := s.List(ctx, "")
	if err != nil {
		return errors.Wrapf(err, "failed to list store")
	}

	// save original value of auto push
	{
		// shadow ctx in this block only
		ctx := WithAutoSync(ctx, false)
		ctx = ctxutil.WithGitCommit(ctx, false)

		// progress bar
		bar := &goprogressbar.ProgressBar{
			Total: int64(len(entries)),
			Width: 120,
		}
		if !ctxutil.IsTerminal(ctx) || out.IsHidden(ctx) {
			bar = nil
		}
		for _, e := range entries {
			// check for context cancelation
			select {
			case <-ctx.Done():
				return errors.New("context canceled")
			default:
			}

			if bar != nil {
				bar.Current++
				bar.Text = fmt.Sprintf("%d of %d secrets reencrypted", bar.Current, bar.Total)
				bar.LazyPrint()
			}

			content, err := s.Get(ctx, e)
			if err != nil {
				out.Red(ctx, "Failed to get current value for %s: %s", e, err)
				continue
			}
			if err := s.Set(ctx, e, content); err != nil {
				out.Red(ctx, "Failed to write %s: %s", e, err)
			}
		}
	}

	if err := s.sync.Commit(ctx, GetReason(ctx)); err != nil {
		if errors.Cause(err) != store.ErrGitNotInit {
			return errors.Wrapf(err, "failed to commit changes to git")
		}
	}

	if !IsAutoSync(ctx) {
		return nil
	}

	return s.reencryptGitPush(ctx)
}

func (s *Store) reencryptGitPush(ctx context.Context) error {
	if err := s.sync.Push(ctx, "", ""); err != nil {
		if errors.Cause(err) == store.ErrGitNotInit {
			msg := "Warning: git is not initialized for this store. Ignoring auto-push option\n" +
				"Run: gopass git init"
			out.Red(ctx, msg)
			return nil
		}
		if errors.Cause(err) == store.ErrGitNoRemote {
			msg := "Warning: git has no remote. Ignoring auto-push option\n" +
				"Run: gopass git remote add origin ..."
			out.Yellow(ctx, msg)
			return nil
		}
		return errors.Wrapf(err, "failed to push change to git remote")
	}
	return nil
}

// Path returns the value of path
func (s *Store) Path() string {
	return s.url.Path
}

// Alias returns the value of alias
func (s *Store) Alias() string {
	return s.alias
}

// Store returns the storage backend used by this store
func (s *Store) Store() backend.Store {
	return s.store
}
