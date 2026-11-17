#import <Foundation/Foundation.h>
#import <FileProvider/FileProvider.h>

NS_ASSUME_NONNULL_BEGIN

// Enumerates the contents of one container by asking the Go side.
@interface SimpleFSEnumerator : NSObject <NSFileProviderEnumerator>

- (instancetype)initWithContainer:(NSFileProviderItemIdentifier)container;

@end

NS_ASSUME_NONNULL_END
