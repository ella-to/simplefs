#import "SimpleFSItem.h"
#import "bridge.h"

@interface SimpleFSItem ()
@property (nonatomic, copy) NSFileProviderItemIdentifier ident;
@property (nonatomic, copy) NSString *name;
@property (nonatomic, copy) NSString *parentID;
@property (nonatomic, strong) UTType *type;
@property (nonatomic, assign) long long size;
@property (nonatomic, assign) BOOL folder;
@property (nonatomic, copy) NSString *version;
@property (nonatomic, assign) int perms;
@property (nonatomic, copy, nullable) NSString *linkTarget;
@end

@implementation SimpleFSItem

- (instancetype)initWithIdentifier:(NSFileProviderItemIdentifier)identifier
                          filename:(NSString *)filename
                          parentID:(NSString *)parentID
                       contentType:(UTType *)contentType
                              size:(long long)size
                          isFolder:(BOOL)isFolder
                           version:(NSString *)version
                             perms:(int)perms {
    if ((self = [super init])) {
        _ident = [identifier copy];
        _name = [filename copy];
        _parentID = [parentID copy];
        _type = contentType;
        _size = size;
        _folder = isFolder;
        _version = [version copy];
        _perms = perms;
    }
    return self;
}

#pragma mark - NSFileProviderItem (required)

- (NSFileProviderItemIdentifier)itemIdentifier {
    return self.ident;
}

// An empty Go parent id is the root; map it to the system root constant.
- (NSFileProviderItemIdentifier)parentItemIdentifier {
    return self.parentID.length ? self.parentID
                                : NSFileProviderRootContainerItemIdentifier;
}

- (NSString *)filename {
    return self.name;
}

- (UTType *)contentType {
    return self.type;
}

// The version must change whenever content or metadata changes, otherwise the
// system serves its cached copy. We feed it the Go-side version (ETag).
- (NSFileProviderItemVersion *)itemVersion {
    NSData *v = [(self.version ?: @"1") dataUsingEncoding:NSUTF8StringEncoding];
    return [[NSFileProviderItemVersion alloc] initWithContentVersion:v
                                                    metadataVersion:v];
}

// Capabilities are derived from the Go perms bitmask. Without the matching bit
// the system never offers (and never calls) that operation, so this is the
// preventive half of the permission model; returning an error from a mutation
// is the reactive half.
- (NSFileProviderItemCapabilities)capabilities {
    NSFileProviderItemCapabilities caps = 0;
    if (self.perms & FPPermRead) {
        caps |= NSFileProviderItemCapabilitiesAllowsReading;
        if (self.folder) {
            caps |= NSFileProviderItemCapabilitiesAllowsContentEnumerating;
        }
    }
    if (self.perms & FPPermWrite)    caps |= NSFileProviderItemCapabilitiesAllowsWriting;
    if (self.perms & FPPermRename)   caps |= NSFileProviderItemCapabilitiesAllowsRenaming;
    if (self.perms & FPPermMove)     caps |= NSFileProviderItemCapabilitiesAllowsReparenting;
    if (self.perms & FPPermDelete)   caps |= NSFileProviderItemCapabilitiesAllowsDeleting;
    if (self.perms & FPPermAddItems) caps |= NSFileProviderItemCapabilitiesAllowsAddingSubItems;
    return caps;
}

- (NSNumber *)documentSize {
    return self.folder ? nil : @(self.size);
}

// Symbolic links are presented as links, with their target.
- (NSString *)symlinkTargetPath {
    return self.linkTarget;
}

#pragma mark - Error mapping

+ (NSError *)errorForCode:(int)code message:(NSString *)message {
    NSDictionary *info = message.length ? @{NSLocalizedDescriptionKey: message} : nil;
    switch (code) {
        case FPCodeNotFound:
            return [NSError errorWithDomain:NSFileProviderErrorDomain
                                       code:NSFileProviderErrorNoSuchItem userInfo:info];
        case FPCodeQuota:
            return [NSError errorWithDomain:NSFileProviderErrorDomain
                                       code:NSFileProviderErrorInsufficientQuota userInfo:info];
        case FPCodeNameConflict:
            return [NSError errorWithDomain:NSFileProviderErrorDomain
                                       code:NSFileProviderErrorFilenameCollision userInfo:info];
        case FPCodeServer:
            return [NSError errorWithDomain:NSFileProviderErrorDomain
                                       code:NSFileProviderErrorServerUnreachable userInfo:info];
        case FPCodeNotPermitted:
            return [NSError errorWithDomain:NSCocoaErrorDomain
                                       code:NSFileWriteNoPermissionError userInfo:info];
        case FPCodeUnsupported:
            return [NSError errorWithDomain:NSCocoaErrorDomain
                                       code:NSFeatureUnsupportedError userInfo:info];
        default:
            return [NSError errorWithDomain:NSCocoaErrorDomain
                                       code:NSFileWriteUnknownError userInfo:info];
    }
}

#pragma mark - Factory helpers

// Map a container identifier to the Go side: the system root and working set
// both correspond to the Go root id (the empty string).
static NSString *GoContainer(NSFileProviderItemIdentifier container) {
    if ([container isEqualToString:NSFileProviderRootContainerItemIdentifier] ||
        [container isEqualToString:NSFileProviderWorkingSetContainerItemIdentifier]) {
        return @"";
    }
    return container;
}

+ (SimpleFSItem *)itemFromDictionary:(NSDictionary *)d {
    NSString *name = d[@"name"];
    BOOL isDir = [d[@"isDir"] boolValue];
    NSString *target = d[@"symlinkTarget"];
    UTType *type = isDir ? UTTypeFolder
                 : target.length ? UTTypeSymbolicLink
                 : ([UTType typeWithFilenameExtension:name.pathExtension] ?: UTTypeData);
    SimpleFSItem *item = [[SimpleFSItem alloc] initWithIdentifier:d[@"id"]
                                       filename:name
                                       parentID:(d[@"parentID"] ?: @"")
                                    contentType:type
                                           size:[d[@"size"] longLongValue]
                                       isFolder:isDir
                                        version:[d[@"version"] description]
                                          perms:[d[@"perms"] intValue]];
    item.linkTarget = target.length ? target : nil;
    return item;
}

+ (NSDictionary *)resultFromGoJSON:(char *)json {
    if (json == NULL) {
        return nil;
    }
    NSData *data = [[NSString stringWithUTF8String:json] dataUsingEncoding:NSUTF8StringEncoding];
    ProviderFree(json);

    id obj = [NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
    return [obj isKindOfClass:[NSDictionary class]] ? obj : nil;
}

+ (SimpleFSItem *)rootItem {
    NSDictionary *res = [self resultFromGoJSON:ProviderRootJSON()];
    NSDictionary *d = res[@"item"];
    NSString *name = d[@"name"] ?: @"root";
    int perms = d ? [d[@"perms"] intValue] : (FPPermRead | FPPermAddItems);
    // Force the system root identifier and an empty parent regardless of what
    // the Go side reports for the root's id/parent.
    return [[SimpleFSItem alloc] initWithIdentifier:NSFileProviderRootContainerItemIdentifier
                                       filename:name
                                       parentID:@""
                                    contentType:UTTypeFolder
                                           size:0
                                       isFolder:YES
                                        version:([d[@"version"] description] ?: @"1")
                                          perms:perms];
}

+ (NSArray<SimpleFSItem *> *)itemsInContainer:(NSFileProviderItemIdentifier)container
                                        token:(NSString *)token
                                        limit:(int)limit
                                    nextToken:(NSString **)nextToken
                                        error:(NSError **)error {
    NSString *goID = GoContainer(container);
    NSDictionary *res = [self resultFromGoJSON:
        ProviderListJSON((char *)goID.UTF8String, (char *)token.UTF8String, limit)];
    *nextToken = nil;
    if (![res[@"ok"] boolValue]) {
        *error = [self errorForCode:[res[@"code"] intValue] message:res[@"msg"]];
        return nil;
    }
    NSMutableArray<SimpleFSItem *> *items = [NSMutableArray array];
    for (NSDictionary *d in res[@"items"]) {
        [items addObject:[self itemFromDictionary:d]];
    }
    NSString *next = res[@"next"];
    if (next.length) {
        *nextToken = next;
    }
    return items;
}

+ (SimpleFSItem *)itemForIdentifier:(NSFileProviderItemIdentifier)identifier {
    NSDictionary *res = [self resultFromGoJSON:ProviderItemJSON((char *)identifier.UTF8String)];
    NSDictionary *d = res[@"item"];
    if (![res[@"ok"] boolValue] || !d) {
        return nil;
    }
    return [self itemFromDictionary:d];
}

@end
