#define main steveApplicationMain
#include "main.m"
#undef main

@interface TestApplication : SteveApplication
@property(nonatomic, assign) NSUInteger failures;
@property(nonatomic, assign) NSUInteger connections;
@end

@implementation TestApplication
- (void)showConnectionFailure:(NSString *)detail { self.failures++; }
- (void)showWindow {}
- (void)connectService { self.connections++; }
@end

@interface TestWebView : NSObject
@property(nonatomic, assign) BOOL ready;
@end
@implementation TestWebView
- (void)evaluateJavaScript:(NSString *)script completionHandler:(void (^)(id, NSError *))completion {
    completion(@(self.ready), nil);
}
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
        TestWebView *content = [[TestWebView alloc] init];
        WKWebView *current = (WKWebView *)content;
        WKWebView *previous = (WKWebView *)[[NSObject alloc] init];
        app.webView = current;
        if ([app respondsToSelector:@selector(setAwaitingWorkspace:)]) [app setValue:@YES forKey:@"awaitingWorkspace"];
        [app webView:current didFinishNavigation:nil];
        check(!app.viewLoaded, @"An empty HTML document was mistaken for a ready workspace");
        content.ready = YES;
        [app webView:current didFinishNavigation:nil];
        check(app.viewLoaded, @"Rendered workspace was not made available");
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

        if ([app respondsToSelector:@selector(setAwaitingWorkspace:)]) [app setValue:@YES forKey:@"awaitingWorkspace"];
        [app webView:current didFinishNavigation:nil];
        check(app.viewLoaded, @"The current completed workspace was not retained");
        check([app respondsToSelector:@selector(webViewWebContentProcessDidTerminate:)], @"Content process termination leaves a blank workspace");
        [app webViewWebContentProcessDidTerminate:previous];
        check(app.viewLoaded && app.failures == 1, @"A stale process exit interrupted the current workspace");
        [app webViewWebContentProcessDidTerminate:current];
        check(!app.viewLoaded && app.failures == 2, @"A terminated view was retained instead of offering recovery");
        app.awaitingWorkspace = YES;
        [app openWorkspace:nil];
        check(app.connections == 0, @"Reopening discarded a page that was still loading");
        app.awaitingWorkspace = NO;
        app.showingFailure = YES;
        [app openWorkspace:nil];
        check(app.connections == 0, @"Reopening hid the recovery instructions");
        [app retryWorkspace:nil];
        check(app.connections == 1, @"Explicit retry did not reconnect the workspace");
        puts("Desktop navigation recovery passed");
    }
    return 0;
}
