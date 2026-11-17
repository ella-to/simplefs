#import "SimpleFSEnumerator.h"
#import "SimpleFSItem.h"
#import "bridge.h"

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

static NSFileProviderSyncAnchor AnchorData(unsigned long long n) {
    return [[NSString stringWithFormat:@"%llu", n] dataUsingEncoding:NSUTF8StringEncoding];
}

- (void)enumerateChangesForObserver:(id<NSFileProviderChangeObserver>)observer
                     fromSyncAnchor:(NSFileProviderSyncAnchor)syncAnchor {
    // Changes are tracked globally and reported through the working set,
    // the enumerator the system uses to keep its whole replica current.
    // Other containers have nothing extra to report.
    if (![self.container isEqualToString:NSFileProviderWorkingSetContainerItemIdentifier]) {
        [observer finishEnumeratingChangesUpToSyncAnchor:AnchorData(ProviderSyncAnchor()) moreComing:NO];
        return;
    }
    NSString *s = [[NSString alloc] initWithData:syncAnchor encoding:NSUTF8StringEncoding];
    unsigned long long from = strtoull(s.UTF8String ?: "0", NULL, 10);
    NSDictionary *res = [SimpleFSItem resultFromGoJSON:ProviderChangesSince(from)];
    if (![res[@"ok"] boolValue]) {
        NSError *err = [res[@"code"] intValue] == FPCodeExpired
            ? [NSError errorWithDomain:NSFileProviderErrorDomain code:NSFileProviderErrorSyncAnchorExpired userInfo:nil]
            : [SimpleFSItem errorForCode:[res[@"code"] intValue] message:res[@"msg"]];
        [observer finishEnumeratingWithError:err];
        return;
    }
    NSMutableArray *updated = [NSMutableArray array];
    for (NSDictionary *d in res[@"updated"]) {
        [updated addObject:[SimpleFSItem itemFromDictionary:d]];
    }
    if (updated.count) {
        [observer didUpdateItems:updated];
    }
    NSArray *deleted = res[@"deleted"];
    if (deleted.count) {
        [observer didDeleteItemsWithIdentifiers:deleted];
    }
    [observer finishEnumeratingChangesUpToSyncAnchor:AnchorData([res[@"anchor"] unsignedLongLongValue])
                                          moreComing:NO];
}

- (void)currentSyncAnchorWithCompletionHandler:(void (^)(NSFileProviderSyncAnchor _Nullable))completionHandler {
    completionHandler(AnchorData(ProviderSyncAnchor()));
}

@end
