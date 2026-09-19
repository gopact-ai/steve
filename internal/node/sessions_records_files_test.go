//go:build unix

package node

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSessionRecordsRefuseUnsafeFilesBeforeSQLite(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal", ".lock"} {
		for _, kind := range []string{"hardlink", "symlink", "shared"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "records")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "sessions.db")
				victim := filepath.Join(t.TempDir(), "private-data")
				want := []byte("must not be changed by sqlite or chmod")
				if err := os.WriteFile(victim, want, 0600); err != nil {
					t.Fatal(err)
				}
				if suffix == "" {
					db, err := sql.Open("sqlite", victim)
					if err != nil {
						t.Fatal(err)
					}
					// Use a real existing database: corrupt bytes can fail before
					// the unsafe open reaches DDL/chmod and hide the regression.
					if err := os.WriteFile(victim, nil, 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec(`CREATE TABLE other_owner(value)`); err != nil {
						t.Fatal(err)
					}
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
					want, err = os.ReadFile(victim)
					if err != nil {
						t.Fatal(err)
					}
				}
				target := path + suffix
				switch kind {
				case "hardlink":
					if err := os.Link(victim, target); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink(victim, target); err != nil {
						t.Fatal(err)
					}
				case "shared":
					if err := os.WriteFile(target, want, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(target, 0644); err != nil {
						t.Fatal(err)
					}
					victim = target
				}
				before, err := os.Stat(victim)
				if err != nil {
					t.Fatal(err)
				}
				store, err := openSessionRecords(path)
				if store != nil {
					_ = store.close()
				}
				if err == nil {
					t.Error("unsafe database or sidecar was accepted")
				}
				got, readErr := os.ReadFile(victim)
				after, statErr := os.Stat(victim)
				if readErr != nil || statErr != nil || !bytes.Equal(got, want) || after.Mode() != before.Mode() {
					t.Errorf("refusal modified an unsafe file: read=%v stat=%v", readErr, statErr)
				}
				if suffix != "" {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Error("SQLite database created before unsafe sidecar was refused")
					}
				}
			})
		}
	}
}

func TestSessionRecordsRefuseUnsafeDirectoryAndOldJSON(t *testing.T) {
	for _, kind := range []string{"shared-directory", "symlink-directory", "old-json", "unknown-file"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "records")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "shared-directory":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink-directory":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				dir = link
			case "old-json":
				if err := os.WriteFile(filepath.Join(dir, "ns_old.json"), []byte(`{"format":1}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-file":
				if err := os.WriteFile(filepath.Join(dir, "unknown"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "sessions.db")
			store, err := openSessionRecords(path)
			if store != nil {
				_ = store.close()
			}
			if err == nil {
				t.Fatal("unsafe or unsupported directory accepted")
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("rejected directory was modified by SQLite")
			}
		})
	}
}

func TestSessionRecordsDatabaseIdentityVersionAndLifetime(t *testing.T) {
	t.Run("exclusive-lifetime", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "records", "sessions.db")
		first, err := openSessionRecords(path)
		if err != nil {
			t.Fatal(err)
		}
		defer first.close()
		second, err := openSessionRecords(path)
		if second != nil {
			_ = second.close()
		}
		if err == nil {
			t.Error("two session services acquired the same records database")
		}
		if err := first.close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := openSessionRecords(path)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.close()
		var version, application int
		if err := reopened.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := reopened.db.QueryRow(`PRAGMA application_id`).Scan(&application); err != nil {
			t.Fatal(err)
		}
		if version != 1 || application == 0 {
			t.Fatalf("database has no explicit owner/version: %d/%d", application, version)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("database not private: %v %v", info, err)
		}
	})
	for _, kind := range []string{"unknown-version", "foreign-schema", "empty-existing"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "records")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "sessions.db")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if kind != "empty-existing" {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`CREATE TABLE foreign_owner(value); PRAGMA user_version=999`); err != nil {
					t.Fatal(err)
				}
				if kind == "foreign-schema" {
					if _, err := db.Exec(`PRAGMA user_version=1`); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			store, err := openSessionRecords(path)
			if store != nil {
				_ = store.close()
			}
			if err == nil {
				t.Error("unsupported database identity/version accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("unsupported database modified before refusal: %v", err)
			}
		})
	}
}

func TestSessionRecordsCreationIsPrivateBeforeSQLite(t *testing.T) {
	if os.Getenv("STEVE_TEST_SESSION_UMASK") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSessionRecordsCreationIsPrivateBeforeSQLite$")
		cmd.Env = append(os.Environ(), "STEVE_TEST_SESSION_UMASK=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated zero-umask process: %v\n%s", err, out)
		}
		return
	}
	// umask is process-global: only change it in this dedicated test process.
	syscall.Umask(0)
	path := filepath.Join(t.TempDir(), "records", "sessions.db")
	files, err := prepareSessionRecordFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 || info.Size() != 0 {
		t.Fatalf("pre-SQLite creation was not private and empty: %v %v", info, err)
	}
	if err := files.close(); err != nil {
		t.Fatal(err)
	}
	// The intentionally uninitialized file is not a supported database.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	store, err := openSessionRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if _, err := store.db.Exec(`INSERT INTO sessions(id,sequence,header) VALUES('test',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".lock", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("SQLite file %s exposed under zero umask: %v %v", suffix, info, err)
		}
	}
}

func TestSessionRecordsMissingSchemaRejectedBeforeWritableOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records", "sessions.db")
	store, err := openSessionRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER session_commands_quota_insert`); err != nil {
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err = openSessionRecords(path)
	if store != nil {
		_ = store.close()
	}
	if err == nil {
		t.Fatal("missing quota enforcement was silently repaired or accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid schema was modified before refusal")
	}
}
