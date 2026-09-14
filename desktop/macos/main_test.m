#define main steveApplicationMain
#include "main.m"
#undef main

@interface TestApplication : SteveApplication
@property(nonatomic, assign) NSUInteger failures;
@end

@implementation TestApplication
- (void)showConnectionFailure:(NSString *)detail { self.failures++; }
@end

static void check(BOOL condition, NSString *message) {
    if (!condition) {
        fprintf(stderr, "%s\n", message.UTF8String);
        exit(1);
    }
}

int main(void) {
    @autoreleasepool {
        TestApplication *app = [[TestApplication alloc] init];
        // Only object identity is needed; no window or WebKit process starts.
        WKWebView *current = (WKWebView *)[[NSObject alloc] init];
        WKWebView *previous = (WKWebView *)[[NSObject alloc] init];
        app.webView = current;
        app.viewLoaded = YES;
        NSError *cancelled = [NSError errorWithDomain:NSURLErrorDomain code:NSURLErrorCancelled userInfo:nil];
        [app webView:current didFailProvisionalNavigation:nil withError:cancelled];
        check(app.viewLoaded && app.failures == 0, @"A replaced navigation discarded the loaded workspace");

        NSError *failed = [NSError errorWithDomain:NSURLErrorDomain code:NSURLErrorNetworkConnectionLost userInfo:nil];
        [app webView:previous didFailProvisionalNavigation:nil withError:failed];
        check(app.viewLoaded && app.failures == 0, @"A stale view interrupted the current workspace");

        check([app respondsToSelector:@selector(webView:didFailNavigation:withError:)], @"Committed navigation failures leave a blank workspace");
        [app webView:current didFailNavigation:nil withError:failed];
        check(!app.viewLoaded && app.failures == 1, @"Failed navigation did not offer recovery");
        [app webView:previous didFinishNavigation:nil];
        check(!app.viewLoaded, @"A stale completion made a failed view reusable");

        [app webView:current didFinishNavigation:nil];
        check(app.viewLoaded, @"The current completed workspace was not retained");
        check([app respondsToSelector:@selector(webViewWebContentProcessDidTerminate:)], @"Content process termination leaves a blank workspace");
        [app webViewWebContentProcessDidTerminate:previous];
        check(app.viewLoaded && app.failures == 1, @"A stale process exit interrupted the current workspace");
        [app webViewWebContentProcessDidTerminate:current];
        check(!app.viewLoaded && app.failures == 2, @"A terminated view was retained instead of offering recovery");
        puts("Desktop navigation recovery passed");
    }
    return 0;
}
