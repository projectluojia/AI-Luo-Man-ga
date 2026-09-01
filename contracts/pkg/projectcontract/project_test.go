package projectcontract_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
)

func validManifest() projectcontract.Manifest {
	return projectcontract.Manifest{
		SchemaVersion: projectcontract.SchemaVersion,
		ID:            "ailuo",
		Dependencies: []projectcontract.Dependency{{
			ID: "demo.pkg", Constraint: "^1.0.0", Source: "path:packages/demo",
		}},
	}
}

func validLock() projectcontract.Lock {
	return projectcontract.Lock{
		SchemaVersion:         projectcontract.SchemaVersion,
		ProjectID:             "ailuo",
		ProjectManifestSHA256: strings.Repeat("c", 64),
		Packages: []projectcontract.LockedPackage{{
			ID: "demo.pkg", Version: "1.2.0", Source: "path:packages/demo",
			ManifestSHA256: strings.Repeat("a", 64), LockSHA256: strings.Repeat("b", 64),
		}},
	}
}

func TestValidateProjectLock(t *testing.T) {
	manifest := validManifest()
	if err := projectcontract.ValidateManifest(manifest); err != nil {
		t.Fatal(err)
	}
	if err := projectcontract.ValidateLock(validLock(), manifest); err != nil {
		t.Fatal(err)
	}
}

func TestValidateProjectLockRequiresDirectDependency(t *testing.T) {
	manifest := validManifest()
	lock := validLock()
	lock.Packages = nil
	if err := projectcontract.ValidateLock(lock, manifest); !errors.Is(err, projectcontract.ErrInvalid) {
		t.Fatalf("ValidateLock = %v, want ErrInvalid", err)
	}
}

func TestValidateProjectLockRejectsInvalidVersionAndDigest(t *testing.T) {
	manifest := validManifest()
	for _, mutate := range []func(*projectcontract.Lock){
		func(lock *projectcontract.Lock) { lock.Packages[0].Version = "1.0" },
		func(lock *projectcontract.Lock) { lock.Packages[0].ManifestSHA256 = "not-a-digest" },
		func(lock *projectcontract.Lock) { lock.Packages[0].Source = "demo.pkg" },
	} {
		lock := validLock()
		mutate(&lock)
		if err := projectcontract.ValidateLock(lock, manifest); !errors.Is(err, projectcontract.ErrInvalid) {
			t.Fatalf("ValidateLock(%+v) = %v, want ErrInvalid", lock.Packages[0], err)
		}
	}
}

func TestProjectDependencyUsesSameVersionSemantics(t *testing.T) {
	if _, err := packagecontract.ParseConstraint("^1.0.0"); err != nil {
		t.Fatal(err)
	}
}
