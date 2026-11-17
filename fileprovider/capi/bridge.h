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

// Per-item permission bits (mirrors fileprovider.Perm). The host maps these to
// NSFileProviderItemCapabilities; an unset bit disables that operation in Finder.
enum {
    FPPermRead     = 1 << 0, // read content / enumerate folder
    FPPermWrite    = 1 << 1, // modify file content
    FPPermRename   = 1 << 2, // change name
    FPPermMove     = 1 << 3, // change parent
    FPPermDelete   = 1 << 4, // delete
    FPPermAddItems = 1 << 5, // create children (folders)
};

// Error classes (mirrors fileprovider.Code). The host maps these to NSError; see
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
    FPCodeExpired      = 8, // sync anchor too old: re-enumerate
};

// Metadata / listing. Each returns a JSON result envelope C string the caller
// frees with ProviderFree.
char *ProviderRootJSON(void);
// ProviderListJSON returns one page (at most limit items; <= 0: all) after
// token ("" for the first page); the envelope's "next" is the following
// page's token, absent on the last page.
char *ProviderListJSON(char *parentID, char *token, int limit);
char *ProviderItemJSON(char *id);

// Content. Streams the file's content into the local file dstPath and
// returns a result envelope with the item ({ok,item} or {ok:false,code,msg}).
char *ProviderFetchToPath(char *id, char *dstPath);

// Mutations. Each returns a JSON result envelope C string freed with
// ProviderFree. Content is passed as a local file path (srcPath, "" for
// none) and streamed by the Go side, so it never sits in memory whole.
char *ProviderCreate(char *name, char *parentID, int isDir, char *srcPath);
char *ProviderUpdate(char *id, char *newName, char *newParentID,
                     int changedContent, int changedName, int changedParent,
                     char *srcPath);
char *ProviderCreateSymlink(char *name, char *parentID, char *target);
char *ProviderDelete(char *id);

// Change tracking. ProviderTrackChanges starts it; notify is called from a
// Go thread whenever changes are available (signal the working set from
// it). ProviderChangesSince returns {ok,updated,deleted,anchor} or a failure
// envelope (FPCodeExpired: re-enumerate).
typedef void (*sfs_notify_fn)(void);
void ProviderTrackChanges(sfs_notify_fn notify);
unsigned long long ProviderSyncAnchor(void);
char *ProviderChangesSince(unsigned long long anchor);

void ProviderSetHostDir(char *path);
void ProviderSetDomain(char *id);
void ProviderFree(void *p);

#ifdef __cplusplus
}
#endif

#endif // SIMPLEFS_BRIDGE_H
