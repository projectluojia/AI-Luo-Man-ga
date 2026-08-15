package qq

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestProvisionerConcurrentlyEnsuresStableIdentity(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := identity.NewService(store)
	provisioner, err := NewProvisioner(service)
	if err != nil {
		t.Fatal(err)
	}
	message := access.InboundMessage{
		AppID: "campus-services", Platform: "qq", PlatformSpaceID: "12345", PlatformUserID: "67890",
	}
	var workers sync.WaitGroup
	errorsSeen := make(chan error, 8)
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errorsSeen <- provisioner.EnsureQQIdentity(t.Context(), message)
		}()
	}
	workers.Wait()
	close(errorsSeen)
	for result := range errorsSeen {
		if result != nil {
			t.Fatalf("并发开通失败：%v", result)
		}
	}
	resolved, err := service.ResolveIdentity(t.Context(), "campus-services", "qq", "12345", "67890")
	if err != nil || resolved.UserID != qqUserID("campus-services", "67890") || resolved.Membership == nil {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
}

func TestProvisionerUsesOneUserAcrossQQSpaces(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-spaces.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := identity.NewService(store)
	provisioner, _ := NewProvisioner(service)
	for _, spaceID := range []string{"private", "12345", "54321"} {
		if err := provisioner.EnsureQQIdentity(t.Context(), access.InboundMessage{
			AppID: "campus-services", Platform: "qq", PlatformSpaceID: spaceID, PlatformUserID: "67890",
		}); err != nil {
			t.Fatal(err)
		}
		resolved, err := service.ResolveIdentity(t.Context(), "campus-services", "qq", spaceID, "67890")
		if err != nil || resolved.UserID != qqUserID("campus-services", "67890") {
			t.Fatalf("space=%s resolved=%#v err=%v", spaceID, resolved, err)
		}
	}
}
