#import <Foundation/Foundation.h>
#import <FileProvider/FileProvider.h>

NS_ASSUME_NONNULL_BEGIN

// Principal class of the extension. Named in the extension's Info.plist under
// NSExtension > NSExtensionPrincipalClass.
@interface FileProviderExtension : NSObject <NSFileProviderReplicatedExtension>

- (instancetype)initWithDomain:(NSFileProviderDomain *)domain;

@end

NS_ASSUME_NONNULL_END
