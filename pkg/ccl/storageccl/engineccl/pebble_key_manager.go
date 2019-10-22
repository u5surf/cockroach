// Copyright 2019 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package engineccl

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl/engineccl/enginepbccl"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/gogo/protobuf/proto"
	"io/ioutil"
	"os"
	"sync"
	"time"
)

const (
	// The key filename for the StoreKeyManager when using plaintext.
	StoreFileNamePlain = "plain"
	// The key id used in KeyInfo for a plaintext "key".
	PlainKeyId = "plain"
	// The length of a real key id.
	KeyIdLength = 32
	// The filename used for writing the data keys by the DataKeyManager.
	KeyRegistryFilename = "COCKROACHDB_DATA_KEYS"
)

// Manages encryption keys. There are two implementations. See encrypted_fs.go for high-level
// context.
type PebbleKeyManager interface {
	// Returns the currently active key. If plaintext should be used it can return nil or a key
	// with encryption_type = Plaintext.
	ActiveKey() (*enginepbccl.SecretKey, error)

	// Gets the key for the given id. Returns an error if the key was not found.
	GetKey(id string) (*enginepbccl.SecretKey, error)
}

// Manages the user-provided keys. Implements PebbleKeyManager.
type StoreKeyManager struct {
	// Initialize the following before calling Load().
	fs                vfs.FS
	activeKeyFilename string
	oldKeyFilename    string

	// Implementation. Both are not nil after a successful call to Load().
	activeKey *enginepbccl.SecretKey
	oldKey    *enginepbccl.SecretKey
}

// Must be called before calling other functions.
func (m *StoreKeyManager) Load() error {
	var err error
	m.activeKey, err = loadKeyFromFile(m.fs, m.activeKeyFilename)
	if err != nil {
		return err
	}
	m.oldKey, err = loadKeyFromFile(m.fs, m.oldKeyFilename)
	if err != nil {
		return err
	}
	return nil
}

func (m *StoreKeyManager) ActiveKey() (*enginepbccl.SecretKey, error) {
	return m.activeKey, nil
}

func (m *StoreKeyManager) GetKey(id string) (*enginepbccl.SecretKey, error) {
	if m.activeKey.Info.KeyId == id {
		return m.activeKey, nil
	}
	if m.oldKey.Info.KeyId == id {
		return m.oldKey, nil
	}
	return nil, fmt.Errorf("store key with id: %s was not found", id)
}

func loadKeyFromFile(fs vfs.FS, filename string) (*enginepbccl.SecretKey, error) {
	now := time.Now().Unix()
	key := &enginepbccl.SecretKey{}
	key.Info = &enginepbccl.KeyInfo{}
	if filename == StoreFileNamePlain {
		key.Info.EncryptionType = enginepbccl.EncryptionType_Plaintext
		key.Info.KeyId = PlainKeyId
		key.Info.CreationTime = now
		key.Info.Source = StoreFileNamePlain
		return key, nil
	}

	f, err := fs.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	// KeyIdLength bytes for the ID, followed by the key.
	keyLength := len(b) - KeyIdLength
	switch keyLength {
	case 16:
		key.Info.EncryptionType = enginepbccl.EncryptionType_AES128_CTR
	case 24:
		key.Info.EncryptionType = enginepbccl.EncryptionType_AES192_CTR
	case 32:
		key.Info.EncryptionType = enginepbccl.EncryptionType_AES256_CTR
	default:
		return nil, fmt.Errorf("store key of unsupported length: %d", keyLength)
	}
	key.Key = b[KeyIdLength:]
	// Hex encoding to make it human readable.
	key.Info.KeyId = hex.EncodeToString(b[:KeyIdLength])
	key.Info.CreationTime = now
	key.Info.Source = filename

	return key, nil
}

// Overridden for testing.
var dkmTimeNow = time.Now

// Manages data keys. Implements PebbleKeyManager. Key rotation does not begin until SetActiveStoreKeyInfo()
// is called.
type DataKeyManager struct {
	// Initialize the following before calling Load().
	fs             vfs.FS
	dbDir          string
	rotationPeriod int64 // seconds

	// Implementation.

	// Initialized in Load()
	registryFilename string

	mu struct {
		sync.Mutex
		// Non-nil after Load()
		keyRegistry *enginepbccl.DataKeysRegistry
		// rotationEnabled => non-nil
		activeKey *enginepbccl.SecretKey
		// Transitions to true when SetActiveStoreKeyInfo() is called for the
		// first time.
		rotationEnabled bool
	}
}

func MakeRegistryProto() *enginepbccl.DataKeysRegistry {
	return &enginepbccl.DataKeysRegistry{
		StoreKeys: make(map[string]*enginepbccl.KeyInfo),
		DataKeys:  make(map[string]*enginepbccl.SecretKey),
	}
}
func (m *DataKeyManager) Load() error {
	m.registryFilename = m.fs.PathJoin(m.dbDir, KeyRegistryFilename)
	_, err := m.fs.Stat(m.registryFilename)
	m.mu.Lock()
	defer m.mu.Unlock()
	if os.IsNotExist(err) {
		// First run.
		m.mu.keyRegistry = MakeRegistryProto()
		return nil
	} else if err != nil {
		return err
	}

	f, err := m.fs.Open(m.registryFilename)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return err
	}
	m.mu.keyRegistry = MakeRegistryProto()
	if err = m.mu.keyRegistry.Unmarshal(b); err != nil {
		return err
	}
	if err = validateRegistry(m.mu.keyRegistry); err != nil {
		return err
	}
	if m.mu.keyRegistry.ActiveDataKeyId != "" {
		key, found := m.mu.keyRegistry.DataKeys[m.mu.keyRegistry.ActiveDataKeyId]
		if !found {
			// This should have resulted in an error in validateRegistry()
			panic("unexpected inconsistent DataKeysRegistry")
		}
		m.mu.activeKey = key
		fmt.Printf("loaded active data key: %s", m.mu.activeKey.Info.String())
	} else {
		fmt.Printf("no active data key yet")
	}
	return nil
}

func (m *DataKeyManager) ActiveKey() (*enginepbccl.SecretKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mu.rotationEnabled {
		now := dkmTimeNow().Unix()
		if now - m.mu.activeKey.Info.CreationTime > m.rotationPeriod {
			keyRegistry := MakeRegistryProto()
			proto.Merge(keyRegistry, m.mu.keyRegistry)
			if err := m.rotateDataKeyAndWrite(keyRegistry); err != nil {
				return nil, err
			}
		}
	}
	return m.mu.activeKey, nil
}

func (m *DataKeyManager) GetKey(id string) (*enginepbccl.SecretKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, found := m.mu.keyRegistry.DataKeys[id]
	if !found {
		return nil, fmt.Errorf("key %s is not found", id)
	}
	return key, nil
}

// Even though there may be a valid ActiveStoreKeyId in the DataKeysRegistry loaded from
// file, key rotation does not start until the first call to the following function. Each
// call to this function will rotate the active data key under the following conditions:
// - there is no active data key.
// - the active store key has changed.
//
// This function should not be called for a read only store.
func (m *DataKeyManager) SetActiveStoreKeyInfo(storeKeyInfo *enginepbccl.KeyInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	prevActiveStoreKey, found := m.mu.keyRegistry.StoreKeys[m.mu.keyRegistry.ActiveStoreKeyId]
	if found && prevActiveStoreKey.KeyId == storeKeyInfo.KeyId && m.mu.activeKey != nil {
		// The active store key has not changed and we already have an active data key,
		// so no need to do anything.
	}
	// For keys other than plaintext, make sure the user is not reusing inactive keys.
	if storeKeyInfo.EncryptionType != enginepbccl.EncryptionType_Plaintext {
		if _, found := m.mu.keyRegistry.StoreKeys[storeKeyInfo.KeyId]; found {
			return fmt.Errorf("new active store key ID %s already exists as an inactive key",
				storeKeyInfo.KeyId)
		}
	}

	// The keyRegistry proto that will replace the current one.
	keyRegistry := MakeRegistryProto()
	proto.Merge(keyRegistry, m.mu.keyRegistry)
	keyRegistry.StoreKeys[storeKeyInfo.KeyId] = storeKeyInfo
	keyRegistry.ActiveStoreKeyId = storeKeyInfo.KeyId
	if storeKeyInfo.EncryptionType == enginepbccl.EncryptionType_Plaintext {
		// Mark all data keys as exposed.
		for _, key := range keyRegistry.DataKeys {
			key.Info.WasExposed = true
		}
	}
	if err := m.rotateDataKeyAndWrite(keyRegistry); err != nil {
		return err
	}
	m.mu.rotationEnabled = true
	return nil
}

func validateRegistry(keyRegistry *enginepbccl.DataKeysRegistry) error {
	if keyRegistry.ActiveStoreKeyId != "" && keyRegistry.StoreKeys[keyRegistry.ActiveStoreKeyId] == nil {
		return fmt.Errorf("active store key %s not found", keyRegistry.ActiveStoreKeyId)
	}
	if keyRegistry.ActiveDataKeyId != "" && keyRegistry.DataKeys[keyRegistry.ActiveDataKeyId] == nil {
		return fmt.Errorf("active data key %s not found", keyRegistry.ActiveDataKeyId)
	}
	return nil
}

// Generates a new data key and adds it to the keyRegistry proto and sets it as the active key.
func generateAndSetNewDateKey(
	keyRegistry *enginepbccl.DataKeysRegistry) (*enginepbccl.SecretKey, error) {
	activeStoreKey := keyRegistry.StoreKeys[keyRegistry.ActiveStoreKeyId]
	if activeStoreKey == nil {
		panic("expected registry with active store key")
	}
	key := &enginepbccl.SecretKey{}
	key.Info = &enginepbccl.KeyInfo{}
	key.Info.EncryptionType = activeStoreKey.EncryptionType
	key.Info.CreationTime = dkmTimeNow().Unix()
	key.Info.Source = "data key manager"
	key.Info.ParentKeyId = activeStoreKey.KeyId

	if activeStoreKey.EncryptionType == enginepbccl.EncryptionType_Plaintext {
		key.Info.KeyId = PlainKeyId
		key.Info.WasExposed = true
	} else {
		var keyLength int
		switch activeStoreKey.EncryptionType {
		case enginepbccl.EncryptionType_AES128_CTR:
			keyLength = 16
		case enginepbccl.EncryptionType_AES192_CTR:
			keyLength = 24
		case enginepbccl.EncryptionType_AES256_CTR:
			keyLength = 32
		default:
			return nil, fmt.Errorf("unknown encryption type %d for key ID %s",
				activeStoreKey.EncryptionType, activeStoreKey.KeyId)
		}
		key.Key = make([]byte, keyLength)
		_, err := rand.Read(key.Key)
		if err != nil {
			return nil, err
		}
		keyId := make([]byte, KeyIdLength)
		if _, err = rand.Read(keyId); err != nil {
			return nil, err
		}
		// Hex encoding to make it human readable.
		key.Info.KeyId = hex.EncodeToString(keyId)
		key.Info.WasExposed = false
	}
	keyRegistry.DataKeys[key.Info.KeyId] = key
	keyRegistry.ActiveDataKeyId = key.Info.KeyId
	return key, nil
}

func (m *DataKeyManager) rotateDataKeyAndWrite(keyRegistry *enginepbccl.DataKeysRegistry) error {
	var newKey *enginepbccl.SecretKey
	var err error
	if newKey, err = generateAndSetNewDateKey(keyRegistry); err != nil {
		return err
	}
	if err := validateRegistry(keyRegistry); err != nil {
		return err
	}
	bytes, err := keyRegistry.Marshal()
	if err != nil {
		return err
	}
	if err = engine.SafeWriteToFile(m.fs, m.dbDir, m.registryFilename, bytes); err != nil {
		return err
	}
	m.mu.keyRegistry = keyRegistry
	m.mu.activeKey = newKey
	return nil
}
