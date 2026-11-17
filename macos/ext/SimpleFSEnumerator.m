#import "SimpleFSEnumerator.h"
#import "SimpleFSItem.h"

@interface SimpleFSEnumerator ()
@property (nonatomic, copy) NSFileProviderItemIdentifier container;
@end

@implementation SimpleFSEnumerator

- (instancetype)initWithContainer:(NSFileProviderItemIdentifier)container {
    if ((self = [super init])) {
        _container = [container copy];
    }
    return self;
}

- (void)invalidate {
}

- (void)enumerateItemsForObserver:(id<NSFileProviderEnumerationObserver>)observer
                   startingAtPage:(NSFileProviderPage)page {
    // Enumerate whatever container this enumerator was created for: the root,
    // the working set (both map to the Go root), or a subfolder the user
    // opened. SimpleFSItem maps the system root/working-set identifiers to the Go
    // root id.
    [observer didEnumerateItems:[SimpleFSItem itemsInContainer:self.container]];
    [observer finishEnumeratingUpToPage:nil];
}

- (void)enumerateChangesForObserver:(id<NSFileProviderChangeObserver>)observer
                     fromSyncAnchor:(NSFileProviderSyncAnchor)syncAnchor {
    // Change tracking is not implemented; the system re-enumerates on demand.
    [observer finishEnumeratingChangesUpToSyncAnchor:syncAnchor moreComing:NO];
}

- (void)currentSyncAnchorWithCompletionHandler:(void (^)(NSFileProviderSyncAnchor _Nullable))completionHandler {
    NSData *anchor = [@"anchor-1" dataUsingEncoding:NSUTF8StringEncoding];
    completionHandler(anchor);
}

@end
