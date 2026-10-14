package testutil

import (
	"testing"

	"github.com/echofog/echofog-core/internal/protection/core/identity"
)

func SetupTestIdentity(t *testing.T) (*identity.ProtectedIdentity, *TestingBadgerStorage) {
	t.Helper()

	testStore := NewTestingBadgerStorage(t)

	im := identity.NewProtectedIdentity(testStore.St)
	passphrase := []byte("super_pro_max_secret_test_password")
	mnemonic, err := im.GenerateNewIdentity(passphrase)
	if err != nil {
		t.Fatalf("Failed to generate test identity: %v", err)
	}

	showedMnemonic, err := mnemonic.Show()
	if err != nil {
		t.Fatalf("Failed to show test identity: %v", err)
	}
	defer mnemonic.Close()

	t.Logf("Test identity generated, mnemonic: \n%s", string(showedMnemonic))

	passphraseToSave := []byte("super_pro_max_secret_test_password")
	if err = im.Save(passphraseToSave); err != nil {
		t.Fatalf("Failed to save test identity to storage: %v", err)
	}

	passphraseToLoad := []byte("super_pro_max_secret_test_password")
	if err = im.Load(passphraseToLoad, false); err != nil {
		t.Fatalf("Failed to unlock test identity: %v", err)
	}

	im.SwitchAccount(0)

	return im, testStore
}
