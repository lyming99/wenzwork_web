//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

static OSStatus wenzwork_keychain_get(const char *service, UInt32 service_len,
                                      const char *account, UInt32 account_len,
                                      void **data, UInt32 *data_len) {
    return SecKeychainFindGenericPassword(NULL, service_len, service,
        account_len, account, data_len, data, NULL);
}

static void wenzwork_keychain_free(void *data, UInt32 data_len) {
    if (data != NULL) {
        if (data_len > 0) memset(data, 0, data_len);
        SecKeychainItemFreeContent(NULL, data);
    }
}

static OSStatus wenzwork_keychain_put(const char *service, UInt32 service_len,
                                      const char *account, UInt32 account_len,
                                      const void *data, UInt32 data_len) {
    SecKeychainItemRef item = NULL;
    OSStatus status = SecKeychainFindGenericPassword(NULL, service_len, service,
        account_len, account, NULL, NULL, &item);
    if (status == errSecItemNotFound) {
        return SecKeychainAddGenericPassword(NULL, service_len, service,
            account_len, account, data_len, data, NULL);
    }
    if (status != errSecSuccess) return status;
    status = SecKeychainItemModifyAttributesAndData(item, NULL, data_len, data);
    CFRelease(item);
    return status;
}

static OSStatus wenzwork_keychain_delete(const char *service, UInt32 service_len,
                                         const char *account, UInt32 account_len) {
    SecKeychainItemRef item = NULL;
    OSStatus status = SecKeychainFindGenericPassword(NULL, service_len, service,
        account_len, account, NULL, NULL, &item);
    if (status == errSecItemNotFound) return errSecSuccess;
    if (status != errSecSuccess) return status;
    status = SecKeychainItemDelete(item);
    CFRelease(item);
    return status;
}
*/
import "C"

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"strings"
	"unsafe"

	"github.com/google/uuid"
)

type keychainSecretStore struct {
	service string
}

func newPlatformSecretStore(path string, deviceID uuid.UUID, identity ed25519.PrivateKey) (secretStore, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("WENZWORK_AGENT_SECRET_STORE")))
	if mode == "file" {
		return newEncryptedFileSecretStore(path, deviceID, identity)
	}
	if mode != "" && mode != "native" {
		return nil, errors.New("WENZWORK_AGENT_SECRET_STORE must be native or file")
	}
	return &keychainSecretStore{service: "wenzwork.device-agent." + deviceID.String()}, nil
}

func (store *keychainSecretStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !validSecretKey(key) {
		return nil, false, errors.New("secret key is invalid")
	}
	service := C.CString(store.service)
	account := C.CString(key)
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	var data unsafe.Pointer
	var size C.UInt32
	status := C.wenzwork_keychain_get(service, C.UInt32(len(store.service)), account, C.UInt32(len(key)), &data, &size)
	if status == C.errSecItemNotFound {
		return nil, false, nil
	}
	if status != C.errSecSuccess || data == nil || size == 0 || uint64(size) > maximumSecretBytes {
		return nil, false, errors.New("read macOS Keychain item")
	}
	defer C.wenzwork_keychain_free(data, size)
	return C.GoBytes(data, C.int(size)), true, nil
}

func (store *keychainSecretStore) Put(ctx context.Context, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validSecretKey(key) {
		return errors.New("secret key is invalid")
	}
	if err := validateSecretValue(value); err != nil {
		return err
	}
	service := C.CString(store.service)
	account := C.CString(key)
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	status := C.wenzwork_keychain_put(service, C.UInt32(len(store.service)), account, C.UInt32(len(key)), unsafe.Pointer(&value[0]), C.UInt32(len(value)))
	if status != C.errSecSuccess {
		return errors.New("write macOS Keychain item")
	}
	return nil
}

func (store *keychainSecretStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validSecretKey(key) {
		return errors.New("secret key is invalid")
	}
	service := C.CString(store.service)
	account := C.CString(key)
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	if status := C.wenzwork_keychain_delete(service, C.UInt32(len(store.service)), account, C.UInt32(len(key))); status != C.errSecSuccess {
		return errors.New("delete macOS Keychain item")
	}
	return nil
}
