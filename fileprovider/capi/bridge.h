// Hand-written declarations for the Go capi bridge's //export functions.
//
// cgo emits these symbols into the c-archive but does not aggregate them into
// the archive's generated header when the bridge lives in an imported package,
// so the Objective-C host imports this file instead. The signatures must match
// the Go //export functions in capi.go exactly.
#ifndef SIMPLEFS_BRIDGE_H
#define SIMPLEFS_BRIDGE_H

#ifdef __cplusplus
extern "C" {
#endif

// Per-item permission bits (mirrors mount.Perm). The host maps these to
// NSFileProviderItemCapabilities; an unset bit disables that operation in Finder.
enum {
    FPPermRead     = 1 << 0, // read content / enumerate folder
    FPPermWrite    = 1 << 1, // modify file content
    FPPermRename   = 1 << 2, // change name
    FPPermMove     = 1 << 3, // change parent
    FPPermDelete   = 1 << 4, // delete
    FPPermAddItems = 1 << 5, // create children (folders)
};

// Error classes (mirrors mount.Code). The host maps these to NSError; see
// SimpleFSItem.m for the NSError domain/code each maps to.
enum {
    FPCodeOK           = 0,
    FPCodeNotFound     = 1,
    FPCodeNotPermitted = 2,
    FPCodeQuota        = 3,
    FPCodeNameConflict = 4,
    FPCodeServer       = 5,
    FPCodeUnsupported  = 6,
    FPCodeIO           = 7,
};

// Metadata / listing. Each returns a JSON result envelope C string the caller
// frees with ProviderFree.
char *ProviderRootJSON(void);
char *ProviderListJSON(char *parentID);
char *ProviderItemJSON(char *id);

// Content. Returns the byte buffer (freed with ProviderFree) and writes the
// length to outLen and the FPCode to outCode. On failure returns NULL; when a
// message is available *outErr is a C string the caller frees with ProviderFree.
void *ProviderContent(char *id, int *outLen, int *outCode, char **outErr);

// Mutations. Each returns a JSON result envelope C string freed with ProviderFree.
char *ProviderCreate(char *name, char *parentID, int isDir, void *data, int n);
char *ProviderUpdate(char *id, char *newName, char *newParentID,
                     int changedContent, int changedName, int changedParent,
                     void *data, int n);
char *ProviderDelete(char *id);

void ProviderSetHostDir(char *path);
void ProviderSetDomain(char *id);
void ProviderFree(void *p);

#ifdef __cplusplus
}
#endif

#endif // SIMPLEFS_BRIDGE_H
