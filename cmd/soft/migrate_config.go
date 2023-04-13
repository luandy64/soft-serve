package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/log"
	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/server/backend"
	"github.com/charmbracelet/soft-serve/server/backend/sqlite"
	"github.com/charmbracelet/soft-serve/server/config"
	"github.com/charmbracelet/soft-serve/server/utils"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

var (
	migrateConfig = &cobra.Command{
		Use:   "migrate-config",
		Short: "Migrate config to new format",
		RunE: func(cmd *cobra.Command, _ []string) error {
			keyPath := os.Getenv("SOFT_SERVE_KEY_PATH")
			reposPath := os.Getenv("SOFT_SERVE_REPO_PATH")
			bindAddr := os.Getenv("SOFT_SERVE_BIND_ADDRESS")
			ctx := cmd.Context()
			cfg := config.DefaultConfig()
			sb, err := sqlite.NewSqliteBackend(ctx, cfg)
			if err != nil {
				return fmt.Errorf("failed to create sqlite backend: %w", err)
			}

			cfg = cfg.WithBackend(sb)

			// Set SSH listen address
			log.Info("Setting SSH listen address...")
			if bindAddr != "" {
				cfg.SSH.ListenAddr = bindAddr
			}

			// Copy SSH host key
			log.Info("Copying SSH host key...")
			if keyPath != "" {
				if err := os.MkdirAll(filepath.Join(cfg.DataPath, "ssh"), 0700); err != nil {
					return fmt.Errorf("failed to create ssh directory: %w", err)
				}

				if err := copyFile(keyPath, filepath.Join(cfg.DataPath, "ssh", filepath.Base(keyPath))); err != nil {
					return fmt.Errorf("failed to copy ssh key: %w", err)
				}

				if err := copyFile(keyPath+".pub", filepath.Join(cfg.DataPath, "ssh", filepath.Base(keyPath))+".pub"); err != nil {
					log.Errorf("failed to copy ssh key: %s", err)
				}

				cfg.SSH.KeyPath = filepath.Join(cfg.DataPath, "ssh", filepath.Base(keyPath))
			}

			// Read config
			log.Info("Reading config repository...")
			r, err := git.Open(filepath.Join(reposPath, "config"))
			if err != nil {
				return fmt.Errorf("failed to open config repo: %w", err)
			}

			head, err := r.HEAD()
			if err != nil {
				return fmt.Errorf("failed to get head: %w", err)
			}

			tree, err := r.TreePath(head, "")
			if err != nil {
				return fmt.Errorf("failed to get tree: %w", err)
			}

			isJson := false // nolint: revive
			te, err := tree.TreeEntry("config.yaml")
			if err != nil {
				te, err = tree.TreeEntry("config.json")
				if err != nil {
					return fmt.Errorf("failed to get config file: %w", err)
				}
				isJson = true
			}

			cc, err := te.Contents()
			if err != nil {
				return fmt.Errorf("failed to get config contents: %w", err)
			}

			var ocfg Config
			if isJson {
				if err := json.Unmarshal(cc, &ocfg); err != nil {
					return fmt.Errorf("failed to unmarshal config: %w", err)
				}
			} else {
				if err := yaml.Unmarshal(cc, &ocfg); err != nil {
					return fmt.Errorf("failed to unmarshal config: %w", err)
				}
			}

			// Set server name
			cfg.Name = ocfg.Name

			// Set server public url
			cfg.SSH.PublicURL = fmt.Sprintf("ssh://%s:%d", ocfg.Host, ocfg.Port)

			// Set server settings
			log.Info("Setting server settings...")
			if cfg.Backend.SetAllowKeyless(ocfg.AllowKeyless) != nil {
				fmt.Fprintf(os.Stderr, "failed to set allow keyless\n")
			}
			anon := backend.ParseAccessLevel(ocfg.AnonAccess)
			if anon >= 0 {
				if err := sb.SetAnonAccess(anon); err != nil {
					fmt.Fprintf(os.Stderr, "failed to set anon access: %s\n", err)
				}
			}

			// Copy repos
			if reposPath != "" {
				log.Info("Copying repos...")
				dirs, err := os.ReadDir(reposPath)
				if err != nil {
					return fmt.Errorf("failed to read repos directory: %w", err)
				}

				for _, dir := range dirs {
					if !dir.IsDir() {
						continue
					}

					if !isGitDir(filepath.Join(reposPath, dir.Name())) {
						continue
					}

					log.Infof("  Copying repo %s", dir.Name())
					if err := os.MkdirAll(filepath.Join(cfg.DataPath, "repos"), 0700); err != nil {
						return fmt.Errorf("failed to create repos directory: %w", err)
					}

					src := utils.SanitizeRepo(filepath.Join(reposPath, dir.Name()))
					dst := utils.SanitizeRepo(filepath.Join(cfg.DataPath, "repos", dir.Name())) + ".git"
					if err := copyDir(src, dst); err != nil {
						return fmt.Errorf("failed to copy repo: %w", err)
					}

					if _, err := sb.CreateRepository(dir.Name(), backend.RepositoryOptions{}); err != nil {
						fmt.Fprintf(os.Stderr, "failed to create repository: %s\n", err)
					}
				}
			}

			// Set repos metadata & collabs
			log.Info("Setting repos metadata & collabs...")
			for _, repo := range ocfg.Repos {
				if err := sb.SetProjectName(repo.Repo, repo.Name); err != nil {
					log.Errorf("failed to set repo name to %s: %s", repo.Repo, err)
				}

				if err := sb.SetDescription(repo.Repo, repo.Note); err != nil {
					log.Errorf("failed to set repo description to %s: %s", repo.Repo, err)
				}

				if err := sb.SetPrivate(repo.Repo, repo.Private); err != nil {
					log.Errorf("failed to set repo private to %s: %s", repo.Repo, err)
				}

				for _, collab := range repo.Collabs {
					if err := sb.AddCollaborator(repo.Repo, collab); err != nil {
						log.Errorf("failed to add repo collab to %s: %s", repo.Repo, err)
					}
				}
			}

			// Create users & collabs
			log.Info("Creating users & collabs...")
			for _, user := range ocfg.Users {
				keys := make(map[string]ssh.PublicKey)
				for _, key := range user.PublicKeys {
					pk, _, err := backend.ParseAuthorizedKey(key)
					if err != nil {
						continue
					}
					ak := backend.MarshalAuthorizedKey(pk)
					keys[ak] = pk
				}

				pubkeys := make([]ssh.PublicKey, 0)
				for _, pk := range keys {
					pubkeys = append(pubkeys, pk)
				}

				username := strings.ToLower(user.Name)
				username = strings.ReplaceAll(username, " ", "-")
				log.Infof("Creating user %q", username)
				if _, err := sb.CreateUser(username, backend.UserOptions{
					Admin:      user.Admin,
					PublicKeys: pubkeys,
				}); err != nil {
					log.Errorf("failed to create user: %s", err)
				}

				for _, repo := range user.CollabRepos {
					if err := sb.AddCollaborator(repo, username); err != nil {
						log.Errorf("failed to add user collab to %s: %s\n", repo, err)
					}
				}
			}

			log.Info("Writing config...")
			defer log.Info("Done!")
			return config.WriteConfig(filepath.Join(cfg.DataPath, "config.yaml"), cfg)
		},
	}
)

// Returns true if path is a directory containing an `objects` directory and a
// `HEAD` file.
func isGitDir(path string) bool {
	stat, err := os.Stat(filepath.Join(path, "objects"))
	if err != nil {
		return false
	}
	if !stat.IsDir() {
		return false
	}

	stat, err = os.Stat(filepath.Join(path, "HEAD"))
	if err != nil {
		return false
	}
	if stat.IsDir() {
		return false
	}

	return true
}

// copyFile copies a single file from src to dst.
func copyFile(src, dst string) error {
	var err error
	var srcfd *os.File
	var dstfd *os.File
	var srcinfo os.FileInfo

	if srcfd, err = os.Open(src); err != nil {
		return err
	}
	defer srcfd.Close() // nolint: errcheck

	if dstfd, err = os.Create(dst); err != nil {
		return err
	}
	defer dstfd.Close() // nolint: errcheck

	if _, err = io.Copy(dstfd, srcfd); err != nil {
		return err
	}
	if srcinfo, err = os.Stat(src); err != nil {
		return err
	}
	return os.Chmod(dst, srcinfo.Mode())
}

// copyDir copies a whole directory recursively.
func copyDir(src string, dst string) error {
	var err error
	var fds []os.DirEntry
	var srcinfo os.FileInfo

	if srcinfo, err = os.Stat(src); err != nil {
		return err
	}

	if err = os.MkdirAll(dst, srcinfo.Mode()); err != nil {
		return err
	}

	if fds, err = os.ReadDir(src); err != nil {
		return err
	}
	for _, fd := range fds {
		srcfp := filepath.Join(src, fd.Name())
		dstfp := filepath.Join(dst, fd.Name())

		if fd.IsDir() {
			if err = copyDir(srcfp, dstfp); err != nil {
				fmt.Println(err)
			}
		} else {
			if err = copyFile(srcfp, dstfp); err != nil {
				fmt.Println(err)
			}
		}
	}
	return nil
}

// Config is the configuration for the server.
type Config struct {
	Name         string       `yaml:"name" json:"name"`
	Host         string       `yaml:"host" json:"host"`
	Port         int          `yaml:"port" json:"port"`
	AnonAccess   string       `yaml:"anon-access" json:"anon-access"`
	AllowKeyless bool         `yaml:"allow-keyless" json:"allow-keyless"`
	Users        []User       `yaml:"users" json:"users"`
	Repos        []RepoConfig `yaml:"repos" json:"repos"`
}

// User contains user-level configuration for a repository.
type User struct {
	Name        string   `yaml:"name" json:"name"`
	Admin       bool     `yaml:"admin" json:"admin"`
	PublicKeys  []string `yaml:"public-keys" json:"public-keys"`
	CollabRepos []string `yaml:"collab-repos" json:"collab-repos"`
}

// RepoConfig is a repository configuration.
type RepoConfig struct {
	Name    string   `yaml:"name" json:"name"`
	Repo    string   `yaml:"repo" json:"repo"`
	Note    string   `yaml:"note" json:"note"`
	Private bool     `yaml:"private" json:"private"`
	Readme  string   `yaml:"readme" json:"readme"`
	Collabs []string `yaml:"collabs" json:"collabs"`
}