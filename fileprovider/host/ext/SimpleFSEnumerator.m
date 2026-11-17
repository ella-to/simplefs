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

// Items per page; the system asks for the next page until we stop
// returning one.
static const int kPageSize = 500;

// The system's initial-page constants (sorted by name or date) both mean
// "from the start"; any other page is a provider token we handed out.
static NSString *TokenForPage(NSFileProviderPage page) {
    if ([page isEqualToData:NSFileProviderInitialPageSortedByName] ||
        [page isEqualToData:NSFileProviderInitialPageSortedByDate]) {
        return @"";
    }
    return [[NSString alloc] initWithData:page encoding:NSUTF8StringEncoding] ?: @"";
}

- (void)enumerateItemsForObserver:(id<NSFileProviderEnumerationObserver>)observer
                   startingAtPage:(NSFileProviderPage)page {
    // Enumerate whatever container this enumerator was created for: the root,
    // the working set (both map to the Go root), or a subfolder the user
    // opened, one page at a time.
    NSString *next = nil;
    NSError *error = nil;
    NSArray *items = [SimpleFSItem itemsInContainer:self.container
                                              token:TokenForPage(page)
                                              limit:kPageSize
                                          nextToken:&next
                                              error:&error];
    if (items == nil) {
        [observer finishEnumeratingWithError:error];
        return;
    }
    [observer didEnumerateItems:items];
    [observer finishEnumeratingUpToPage:[next dataUsingEncoding:NSUTF8StringEncoding]];
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
