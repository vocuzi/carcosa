package carcosa

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/juju/fslock"
	"github.com/reconquest/karma-go"
	"github.com/seletskiy/carcosa/pkg/carcosa/auth"

	git "gopkg.in/src-d/go-git.v4"
	git_config "gopkg.in/src-d/go-git.v4/config"
	git_plumbing "gopkg.in/src-d/go-git.v4/plumbing"
	git_transport "gopkg.in/src-d/go-git.v4/plumbing/transport"
)

type repo struct {
	path string
	git  *git.Repository

	mutex struct {
		path   string
		handle *fslock.Lock
	}
}

func initialize(
	path string,
	remote string,
	url string,
	ns string,
) (*repo, error) {
	log.Infof("{init} %s (%s: %s)", path, remote, url)

	facts := karma.
		Describe("path", path).
		Describe("url", url)

	git, err := git.PlainInit(path, false)
	if err != nil {
		return nil, facts.Format(
			err,
			"unable to init git repository",
		)
	}

	_, err = git.CreateRemote(&git_config.RemoteConfig{
		URLs:  []string{url},
		Name:  remote,
		Fetch: []git_config.RefSpec{git_config.RefSpec(refspec(ns).to())},
	})
	if err != nil {
		return nil, facts.Describe("remote", remote).Format(
			err,
			"unable to create remote",
		)
	}

	return &repo{
		path: path,
		git:  git,
	}, nil
}

func clone(
	url string,
	remote string,
	path string,
	auth auth.Auth,
) (*repo, error) {
	method, err := auth.Get(url)
	if err != nil {
		return nil, err
	}

	log.Infof("{clone} %s -> %s", url, path)

	git, err := git.PlainClone(path, false, &git.CloneOptions{
		NoCheckout: true,
		RemoteName: remote,
		Auth:       method,
		URL:        url,
	})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to clone git repository %q to %q", url, path,
		)
	}

	return &repo{
		path: path,
		git:  git,
	}, nil
}

func open(path string) (*repo, error) {
	git, err := git.PlainOpen(path)
	if err != nil {
		return nil, karma.Format(err, "unable to open git repository %q", path)
	}

	return &repo{
		path: path,
		git:  git,
	}, nil
}

func (repo *repo) update(ref ref) error {
	log.Debugf("{update} %s > %s", ref.hash, ref.name)

	err := repo.git.Storer.SetReference(
		git_plumbing.NewReferenceFromStrings(ref.name, ref.hash),
	)
	if err != nil {
		return karma.Format(
			err,
			"unable to update reference %q -> %q",
			ref.name,
			ref.hash,
		)
	}

	return nil
}

func (repo *repo) delete(ref ref) error {
	log.Tracef("{delete} %s - %s", ref.hash, ref.name)

	err := repo.git.Storer.RemoveReference(
		git_plumbing.ReferenceName(ref.name),
	)
	if err != nil {
		return karma.Format(
			err,
			"unable to delete reference %q",
			ref.name,
		)
	}

	return nil
}

func (repo *repo) write(data []byte) (string, error) {
	var blob git_plumbing.MemoryObject

	blob.SetType(git_plumbing.BlobObject)
	blob.Write(data)

	hash, err := repo.git.Storer.SetEncodedObject(&blob)
	if err != nil {
		return "", karma.Format(
			err,
			"unable to set encoded object (len=%d)",
			len(data),
		)
	}

	return hash.String(), nil
}

func (repo *repo) list(ns string) (refs, error) {
	log.Tracef("{list} %s ?", ns)

	list, err := repo.git.References()
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to list references",
		)
	}

	var refs refs

	defer list.Close()
	defer func() { log.Tracef("{list} %s = %d refs", ns, len(refs)) }()

	return refs, list.ForEach(
		func(reference *git_plumbing.Reference) error {
			ref := ref{
				name: reference.Name().String(),
				hash: reference.Hash().String(),
			}

			if !strings.HasPrefix(ref.name, ns) {
				return nil
			}

			refs = append(refs, ref)

			return nil
		},
	)
}

func (repo *repo) auth(
	name string,
	auth auth.Auth,
) (git_transport.AuthMethod, error) {
	remote, err := repo.git.Remote(name)
	if err != nil {
		return nil, err
	}

	url := remote.Config().URLs[0]

	log.Debugf("{auth} remote %q | url %q", name, url)

	method, err := auth.Get(url)
	if err != nil {
		return nil, err
	}

	return method, nil
}

func (repo *repo) pull(name string, spec refspec, auth auth.Auth) error {
	log.Debugf("{pull} %s %s", name, spec.to())

	method, err := repo.auth(name, auth)
	if err != nil {
		return err
	}

	err = repo.git.Fetch(&git.FetchOptions{
		Auth:       method,
		RemoteName: name,
		RefSpecs:   []git_config.RefSpec{git_config.RefSpec(spec.to())},
	})
	switch err {
	case nil:
		return nil
	case git.NoErrAlreadyUpToDate:
		return nil
	case git_transport.ErrEmptyRemoteRepository:
		log.Infof("{pull} remote repository is empty")
		return nil
	default:
		return karma.Format(
			err,
			"unable to fetch remote %q",
			name,
		)
	}
}

func (repo *repo) push(name string, spec refspec, auth auth.Auth) error {
	log.Debugf("{push} %s %s", name, spec.from())

	method, err := repo.auth(name, auth)
	if err != nil {
		return err
	}

	err = repo.git.Push(&git.PushOptions{
		Auth:       method,
		RemoteName: name,
		RefSpecs:   []git_config.RefSpec{git_config.RefSpec(spec.from())},
		Prune:      true,
	})
	switch err {
	case nil:
		return nil
	case git.NoErrAlreadyUpToDate:
		log.Infof("{push} remote repository is up-to-date")
		return nil
	default:
		return karma.Format(
			err,
			"unable to push to remote %q",
			name,
		)
	}
}

func (repo *repo) cat(hash string) ([]byte, error) {
	log.Tracef("{cat} %s ?", hash)

	blob, err := repo.git.BlobObject(git_plumbing.NewHash(hash))
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to get blob %q",
			hash,
		)
	}

	reader, err := blob.Reader()
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to get reader for blob %q",
			hash,
		)
	}

	data, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to read blob contents %q",
			hash,
		)
	}

	log.Tracef("{cat} %s = %d bytes", hash, len(data))

	return data, nil
}

func (repo *repo) lock() error {
	config, err := repo.git.Config()
	if err != nil {
		return karma.Format(
			err,
			"unable to get git config",
		)
	}

	if config.Core.IsBare {
		repo.mutex.path = filepath.Join(repo.path, "carcosa.lock")
	} else {
		repo.mutex.path = filepath.Join(repo.path, ".git", "carcosa.lock")
	}

	log.Tracef("{lock} obtaining exclusive lock %q", repo.mutex.path)

	repo.mutex.handle = fslock.New(repo.mutex.path)

	err = repo.mutex.handle.TryLock()
	if err != nil {
		return karma.Format(
			err,
			"unable to obtain exclusive lock %q",
			repo.mutex.path,
		)
	}

	return nil
}

func (repo *repo) unlock() error {
	err := os.Remove(repo.mutex.path)
	if err != nil {
		return karma.Format(
			err,
			"unable to remove lock file %q",
			repo.mutex.path,
		)
	}

	err = repo.mutex.handle.Unlock()
	if err != nil {
		return karma.Format(
			err,
			"unable to release exclusive repository lock %q",
			repo.mutex.path,
		)
	}

	return nil
}
