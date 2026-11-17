#import "FileProviderExtension.h"
#import "SimpleFSEnumerator.h"
#import "SimpleFSItem.h"
#import "bridge.h"

@interface FileProviderExtension ()
@property (nonatomic, strong) NSFileProviderDomain *domain;
@end

@implementation FileProviderExtension

- (instancetype)initWithDomain:(NSFileProviderDomain *)domain {
    if ((self = [super init])) {
        _domain = domain;

        // Give the Go side a writable directory: the extension's sandbox home,
        // which maps to ~/Library/Containers/to.ella.simplefs.ext/Data/ and is
        // readable from outside. Providers can place logs or caches there.
        // (containerURLForSecurityApplicationGroupIdentifier: returns nil under
        // ad-hoc signing, so we avoid it.)
        ProviderSetHostDir((char *)NSHomeDirectory().UTF8String);

        // Tell the Go side which domain this process serves. The system runs
        // one extension instance per domain, so this scopes per-mount side
        // effects.
        ProviderSetDomain((char *)domain.identifier.UTF8String);
    }
    return self;
}

- (void)invalidate {
}

#pragma mark - Helpers

// Map a parent identifier to the Go side: the root container is the Go root id
// (the empty string); any other identifier passes through unchanged.
static NSString *GoParent(NSFileProviderItemIdentifier parent) {
    if (parent == nil ||
        [parent isEqualToString:NSFileProviderRootContainerItemIdentifier]) {
        return @"";
    }
    return parent;
}

// Complete a create/modify-style call from a Go result envelope (which it
// frees): hand back the item on success, or the mapped NSError on failure.
static void CompleteMutation(char *json,
        void (^completionHandler)(NSFileProviderItem, NSFileProviderItemFields, BOOL, NSError *)) {
    NSDictionary *res = [SimpleFSItem resultFromGoJSON:json];
    if ([res[@"ok"] boolValue] && res[@"item"]) {
        // No fields left unapplied, no need for the system to re-fetch content.
        completionHandler([SimpleFSItem itemFromDictionary:res[@"item"]], 0, NO, nil);
    } else {
        completionHandler(nil, 0, NO,
            [SimpleFSItem errorForCode:[res[@"code"] intValue] message:res[@"msg"]]);
    }
}

#pragma mark - Metadata

- (NSProgress *)itemForIdentifier:(NSFileProviderItemIdentifier)identifier
                          request:(NSFileProviderRequest *)request
                completionHandler:(void (^)(NSFileProviderItem _Nullable, NSError * _Nullable))completionHandler {
    if ([identifier isEqualToString:NSFileProviderRootContainerItemIdentifier] ||
        [identifier isEqualToString:NSFileProviderWorkingSetContainerItemIdentifier]) {
        completionHandler([SimpleFSItem rootItem], nil);
        return [NSProgress progressWithTotalUnitCount:0];
    }

    SimpleFSItem *item = [SimpleFSItem itemForIdentifier:identifier];
    if (item) {
        completionHandler(item, nil);
    } else {
        completionHandler(nil, [NSError errorWithDomain:NSFileProviderErrorDomain
                                                   code:NSFileProviderErrorNoSuchItem
                                               userInfo:nil]);
    }
    return [NSProgress progressWithTotalUnitCount:0];
}

#pragma mark - Content

- (NSProgress *)fetchContentsForItemWithIdentifier:(NSFileProviderItemIdentifier)itemIdentifier
                                           version:(NSFileProviderItemVersion *)requestedVersion
                                           request:(NSFileProviderRequest *)request
                                 completionHandler:(void (^)(NSURL * _Nullable, NSFileProviderItem _Nullable, NSError * _Nullable))completionHandler {
    SimpleFSItem *item = [SimpleFSItem itemForIdentifier:itemIdentifier];
    if (item == nil) {
        completionHandler(nil, nil, [NSError errorWithDomain:NSFileProviderErrorDomain
                                                        code:NSFileProviderErrorNoSuchItem
                                                    userInfo:nil]);
        return [NSProgress progressWithTotalUnitCount:0];
    }

    int len = 0, code = 0;
    char *errMsg = NULL;
    void *buf = ProviderContent((char *)[itemIdentifier UTF8String], &len, &code, &errMsg);
    if (buf == NULL) {
        NSString *msg = errMsg ? [NSString stringWithUTF8String:errMsg] : nil;
        if (errMsg) {
            ProviderFree(errMsg);
        }
        completionHandler(nil, nil, [SimpleFSItem errorForCode:code message:msg]);
        return [NSProgress progressWithTotalUnitCount:0];
    }

    NSData *data = [NSData dataWithBytes:buf length:len];
    ProviderFree(buf);

    NSURL *tmpURL = [[NSFileManager.defaultManager temporaryDirectory]
                        URLByAppendingPathComponent:[NSUUID UUID].UUIDString];
    NSError *writeErr = nil;
    if (![data writeToURL:tmpURL options:NSDataWritingAtomic error:&writeErr]) {
        completionHandler(nil, nil, writeErr);
        return [NSProgress progressWithTotalUnitCount:0];
    }

    completionHandler(tmpURL, item, nil);
    return [NSProgress progressWithTotalUnitCount:0];
}

#pragma mark - Enumeration

- (id<NSFileProviderEnumerator>)enumeratorForContainerItemIdentifier:(NSFileProviderItemIdentifier)containerItemIdentifier
                                                            request:(NSFileProviderRequest *)request
                                                              error:(NSError **)error {
    return [[SimpleFSEnumerator alloc] initWithContainer:containerItemIdentifier];
}

#pragma mark - Mutations (user create / update / delete -> Go)

// Read the optional content URL the system hands us into an NSData.
static NSData *DataFromURL(NSURL *url) {
    if (url == nil) {
        return nil;
    }
    return [NSData dataWithContentsOfURL:url];
}

- (NSProgress *)createItemBasedOnTemplate:(NSFileProviderItem)itemTemplate
                                   fields:(NSFileProviderItemFields)fields
                                 contents:(NSURL *)url
                                  options:(NSFileProviderCreateItemOptions)options
                                  request:(NSFileProviderRequest *)request
                        completionHandler:(void (^)(NSFileProviderItem _Nullable, NSFileProviderItemFields, BOOL, NSError * _Nullable))completionHandler {
    BOOL isFolder = [itemTemplate.contentType conformsToType:UTTypeFolder];
    NSData *data = isFolder ? nil : DataFromURL(url);
    NSString *parent = GoParent(itemTemplate.parentItemIdentifier);

    char *json = ProviderCreate((char *)itemTemplate.filename.UTF8String,
                                (char *)parent.UTF8String,
                                isFolder ? 1 : 0,
                                (void *)data.bytes,
                                (int)data.length);
    CompleteMutation(json, completionHandler);
    return [NSProgress progressWithTotalUnitCount:0];
}

- (NSProgress *)modifyItem:(NSFileProviderItem)item
               baseVersion:(NSFileProviderItemVersion *)version
              changedFields:(NSFileProviderItemFields)changedFields
                  contents:(NSURL *)newContents
                   options:(NSFileProviderModifyItemOptions)options
                   request:(NSFileProviderRequest *)request
         completionHandler:(void (^)(NSFileProviderItem _Nullable, NSFileProviderItemFields, BOOL, NSError * _Nullable))completionHandler {
    BOOL contentChanged = (changedFields & NSFileProviderItemContents) != 0;
    BOOL nameChanged = (changedFields & NSFileProviderItemFilename) != 0;
    BOOL parentChanged = (changedFields & NSFileProviderItemParentItemIdentifier) != 0;
    NSData *data = contentChanged ? DataFromURL(newContents) : nil;
    NSString *newParent = parentChanged ? GoParent(item.parentItemIdentifier) : @"";

    char *json = ProviderUpdate((char *)item.itemIdentifier.UTF8String,
                                (char *)item.filename.UTF8String,
                                (char *)newParent.UTF8String,
                                contentChanged ? 1 : 0,
                                nameChanged ? 1 : 0,
                                parentChanged ? 1 : 0,
                                (void *)data.bytes,
                                (int)data.length);
    CompleteMutation(json, completionHandler);
    return [NSProgress progressWithTotalUnitCount:0];
}

- (NSProgress *)deleteItemWithIdentifier:(NSFileProviderItemIdentifier)identifier
                             baseVersion:(NSFileProviderItemVersion *)version
                                 options:(NSFileProviderDeleteItemOptions)options
                                 request:(NSFileProviderRequest *)request
                       completionHandler:(void (^)(NSError * _Nullable))completionHandler {
    NSDictionary *res = [SimpleFSItem resultFromGoJSON:ProviderDelete((char *)identifier.UTF8String)];
    if ([res[@"ok"] boolValue]) {
        completionHandler(nil);
    } else {
        completionHandler([SimpleFSItem errorForCode:[res[@"code"] intValue] message:res[@"msg"]]);
    }
    return [NSProgress progressWithTotalUnitCount:0];
}

@end
