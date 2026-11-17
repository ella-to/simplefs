#import <Foundation/Foundation.h>
#import <FileProvider/FileProvider.h>
#import <UniformTypeIdentifiers/UniformTypeIdentifiers.h>

NS_ASSUME_NONNULL_BEGIN

// A single item (file, folder, or the root container) exposed to the system.
// Its capabilities are derived from the "perms" bitmask the Go side provides
// (see FPPerm* in bridge.h), so the provider — not this file — decides what
// the user may do with each item.
@interface SimpleFSItem : NSObject <NSFileProviderItem>

- (instancetype)initWithIdentifier:(NSFileProviderItemIdentifier)identifier
                          filename:(NSString *)filename
                          parentID:(NSString *)parentID
                       contentType:(UTType *)contentType
                              size:(long long)size
                          isFolder:(BOOL)isFolder
                           version:(NSString *)version
                             perms:(int)perms;

// The root container item (queries the Go side for its name and perms).
+ (SimpleFSItem *)rootItem;

// Build an item from a Go item dictionary
// ({id,name,parentID,size,isDir,version,perms}).
+ (SimpleFSItem *)itemFromDictionary:(NSDictionary *)dict;

// Parse a Go result-envelope JSON C string ({ok,item,items,code,msg}) into a
// dictionary. Frees the C string. Returns nil on a parse failure.
+ (nullable NSDictionary *)resultFromGoJSON:(char *)json;

// Children of a container (queries the Go side). The system root and working
// set are mapped to the Go root id.
+ (NSArray<SimpleFSItem *> *)itemsInContainer:(NSFileProviderItemIdentifier)container;

// Look up a single item by identifier (queries the Go side). nil if missing.
+ (nullable SimpleFSItem *)itemForIdentifier:(NSFileProviderItemIdentifier)identifier;

// Map a Go result-envelope error (FPCode + message) to an NSError the system
// understands, so the failure can surface in Finder.
+ (NSError *)errorForCode:(int)code message:(nullable NSString *)message;

@end

NS_ASSUME_NONNULL_END
