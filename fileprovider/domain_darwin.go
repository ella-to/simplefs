//go:build darwin && cgo

package fileprovider

// This file is the host-side (container app) counterpart to the extension-side
// capi bridge: it manages File Provider domains from Go by wrapping
// NSFileProviderManager (addDomain / removeDomain / getDomains /
// getUserVisibleURL), so a normal Go program can mount and unmount folders
// without writing any Objective-C.
//
// It must run from a properly signed app bundle that embeds the File Provider
// extension and carries the matching App Group entitlement; otherwise macOS
// rejects addDomain. See the repository Makefile and host/app.

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=13.0
#cgo LDFLAGS: -framework Foundation -framework FileProvider

#import <Foundation/Foundation.h>
#import <FileProvider/FileProvider.h>
#include <stdlib.h>
#include <string.h>

static char *sfs_cdup(NSString *s) { return s ? strdup(s.UTF8String) : NULL; }

// sfs_mount adds (or reuses) a domain and resolves its user-visible root
// path. Returns 0 on success with *outPath set; on failure returns the NSError
// code and sets *outErr. The caller frees both strings with free().
//
// addDomain reports an error when a domain with the same identifier already
// exists, but the domain is still usable, so we always try to resolve the URL
// and only surface an error when that also fails.
static int sfs_mount(const char *cid, const char *cname, char **outPath, char **outErr) {
    @autoreleasepool {
        NSString *ident = [NSString stringWithUTF8String:cid];
        NSString *name  = [NSString stringWithUTF8String:cname];
        NSFileProviderDomain *domain =
            [[NSFileProviderDomain alloc] initWithIdentifier:ident displayName:name];

        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        __block NSError *addErr = nil;
        [NSFileProviderManager addDomain:domain completionHandler:^(NSError *e) {
            addErr = e;
            dispatch_semaphore_signal(sem);
        }];
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);

        NSFileProviderManager *mgr = [NSFileProviderManager managerForDomain:domain];
        __block NSURL *url = nil;
        __block NSError *urlErr = nil;
        if (mgr) {
            dispatch_semaphore_t sem2 = dispatch_semaphore_create(0);
            [mgr getUserVisibleURLForItemIdentifier:NSFileProviderRootContainerItemIdentifier
                                  completionHandler:^(NSURL *u, NSError *e) {
                url = u; urlErr = e;
                dispatch_semaphore_signal(sem2);
            }];
            dispatch_semaphore_wait(sem2, DISPATCH_TIME_FOREVER);
        }

        if (url) {
            if (outPath) *outPath = sfs_cdup(url.path);
            return 0;
        }
        NSError *err = addErr ? addErr : urlErr;
        if (outErr) *outErr = sfs_cdup(err ? err.localizedDescription : @"unknown error");
        return err ? (int)err.code : -1;
    }
}

// sfs_list returns all File Provider domains owned by this app's extension as
// a JSON array string [{"id":...,"name":...},...] in *outJSON (freed by the
// caller). Returns 0 on success; on failure returns the NSError code and sets
// *outErr.
static int sfs_list(char **outJSON, char **outErr) {
    @autoreleasepool {
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        __block NSError *outE = nil;
        __block NSString *jsonStr = @"[]";
        [NSFileProviderManager getDomainsWithCompletionHandler:^(NSArray<NSFileProviderDomain *> *domains, NSError *e) {
            if (e) { outE = e; dispatch_semaphore_signal(sem); return; }
            NSMutableArray *arr = [NSMutableArray array];
            for (NSFileProviderDomain *d in domains) {
                [arr addObject:@{@"id": d.identifier ?: @"", @"name": d.displayName ?: @""}];
            }
            NSData *data = [NSJSONSerialization dataWithJSONObject:arr options:0 error:nil];
            if (data) {
                jsonStr = [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
            }
            dispatch_semaphore_signal(sem);
        }];
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        if (outE) {
            if (outErr) *outErr = sfs_cdup(outE.localizedDescription);
            return (int)outE.code;
        }
        if (outJSON) *outJSON = sfs_cdup(jsonStr);
        return 0;
    }
}

// sfs_unmount_all removes every domain owned by this app's extension. Returns
// 0 on success; on failure returns the NSError code and sets *outErr.
static int sfs_unmount_all(char **outErr) {
    @autoreleasepool {
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        __block NSError *outE = nil;
        [NSFileProviderManager removeAllDomainsWithCompletionHandler:^(NSError *e) {
            outE = e;
            dispatch_semaphore_signal(sem);
        }];
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        if (outE) {
            if (outErr) *outErr = sfs_cdup(outE.localizedDescription);
            return (int)outE.code;
        }
        return 0;
    }
}

// sfs_unmount removes the domain with the given identifier. Returns 0 on
// success (also when no such domain exists); on failure returns the NSError
// code and sets *outErr (freed by the caller).
static int sfs_unmount(const char *cid, char **outErr) {
    @autoreleasepool {
        NSString *ident = [NSString stringWithUTF8String:cid];
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        __block NSError *outE = nil;
        [NSFileProviderManager getDomainsWithCompletionHandler:^(NSArray<NSFileProviderDomain *> *domains, NSError *e) {
            NSFileProviderDomain *target = nil;
            for (NSFileProviderDomain *d in domains) {
                if ([d.identifier isEqualToString:ident]) { target = d; break; }
            }
            if (!target) { dispatch_semaphore_signal(sem); return; }
            [NSFileProviderManager removeDomain:target completionHandler:^(NSError *re) {
                outE = re;
                dispatch_semaphore_signal(sem);
            }];
        }];
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        if (outE) {
            if (outErr) *outErr = sfs_cdup(outE.localizedDescription);
            return (int)outE.code;
        }
        return 0;
    }
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"unsafe"
)

// AddDomain adds a File Provider domain with the given identifier and
// display name — the name shown in Finder's sidebar — and returns its
// user-visible root path, which lives under ~/Library/CloudStorage. macOS,
// not the caller, chooses that location; AddDomainAt symlinks it elsewhere.
//
// Adding a domain that already exists is treated as success and returns its
// current path. Newly added third-party domains start DISABLED on macOS
// 15.2+; AddDomain still succeeds, but the folder is unusable until the user
// enables it once under System Settings > General > Login Items & Extensions
// > File Providers.
func AddDomain(ctx context.Context, domainID, displayName string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if displayName == "" {
		displayName = domainID
	}
	cid := C.CString(domainID)
	defer C.free(unsafe.Pointer(cid))
	cname := C.CString(displayName)
	defer C.free(unsafe.Pointer(cname))

	var outPath, outErr *C.char
	code := C.sfs_mount(cid, cname, &outPath, &outErr)
	if outPath != nil {
		path := C.GoString(outPath)
		C.free(unsafe.Pointer(outPath))
		return path, nil
	}
	return "", domainErr("add domain "+domainID, code, outErr)
}

// Domains returns every domain owned by this app's extension. Use it to
// rediscover domains that outlived the process (e.g. after a restart) so
// they can be managed or removed again.
func Domains(ctx context.Context) ([]Domain, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var outJSON, outErr *C.char
	if code := C.sfs_list(&outJSON, &outErr); code != 0 {
		return nil, domainErr("list domains", code, outErr)
	}
	var raw []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if outJSON != nil {
		s := C.GoString(outJSON)
		C.free(unsafe.Pointer(outJSON))
		if err := json.Unmarshal([]byte(s), &raw); err != nil {
			return nil, fmt.Errorf("fileprovider: parse domains: %w", err)
		}
	}
	out := make([]Domain, 0, len(raw))
	for _, r := range raw {
		out = append(out, Domain{ID: r.ID, DisplayName: r.Name})
	}
	return out, nil
}

// RemoveAllDomains removes every domain owned by this app's extension,
// including orphans left behind by previous runs.
func RemoveAllDomains(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var outErr *C.char
	if code := C.sfs_unmount_all(&outErr); code != 0 {
		return domainErr("remove all domains", code, outErr)
	}
	return nil
}

// RemoveDomain removes the domain with the given identifier. Removing a
// domain that does not exist is a no-op and returns nil.
func RemoveDomain(ctx context.Context, domainID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cid := C.CString(domainID)
	defer C.free(unsafe.Pointer(cid))
	var outErr *C.char
	if code := C.sfs_unmount(cid, &outErr); code != 0 {
		return domainErr("remove domain "+domainID, code, outErr)
	}
	return nil
}

// domainErr builds the error for a failed NSFileProviderManager call,
// freeing the C message.
func domainErr(what string, code C.int, msg *C.char) error {
	text := "unknown error"
	if msg != nil {
		text = C.GoString(msg)
		C.free(unsafe.Pointer(msg))
	}
	return fmt.Errorf("fileprovider: %s failed (code %d): %s", what, int(code), text)
}
