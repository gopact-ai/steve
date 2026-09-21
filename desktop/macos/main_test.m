#define main steveApplicationMain
#include "main.m"
#undef main

@interface TestApplication : SteveApplication
@property(nonatomic, assign) NSUInteger failures;
@property(nonatomic, assign) NSUInteger connections;
@property(nonatomic, assign) NSUInteger windowCloses;
@end

@implementation TestApplication
- (void)showConnectionFailure:(NSString *)detail { self.failures++; }
- (void)showWindow {}
- (void)connectService { self.connections++; }
- (void)closeWindow { self.windowCloses++; }
@end

@interface TestWebView : NSObject
@property(nonatomic, assign) BOOL ready;
/** What the page reports when asked to close its frontmost panel. */
@property(nonatomic, assign) BOOL layerClosed;
@property(nonatomic, assign) NSUInteger closeRequests;
@property(nonatomic, strong) NSURLRequest *loadedRequest;
@end
@implementation TestWebView
- (void)loadRequest:(NSURLRequest *)request { self.loadedRequest = request; }
- (void)evaluateJavaScript:(NSString *)script completionHandler:(void (^)(id, NSError *))completion {
    if ([script containsString:@"steveCloseLayer"]) {
        self.closeRequests++;
        completion(@(self.layerClosed), nil);
        return;
    }
    completion(@(self.ready), nil);
}
@end

@interface TestOrigin : NSObject
@property(nonatomic, copy) NSString *protocol;
@property(nonatomic, copy) NSString *host;
@property(nonatomic, assign) NSInteger port;
@end
@implementation TestOrigin
@end

@interface TestFrame : NSObject
@property(nonatomic, assign, getter=isMainFrame) BOOL mainFrame;
@property(nonatomic, strong) NSURLRequest *request;
@property(nonatomic, strong) TestOrigin *securityOrigin;
@end
@implementation TestFrame
@end

@interface TestNavigation : NSObject
@property(nonatomic, strong) NSURLRequest *request;
@property(nonatomic, strong) TestFrame *sourceFrame;
@property(nonatomic, strong) TestFrame *targetFrame;
@property(nonatomic, assign) WKNavigationType navigationType;
@end
@implementation TestNavigation
@end

static TestFrame *frame(NSString *address, BOOL mainFrame) {
    NSURL *url = [NSURL URLWithString:address];
    TestFrame *frame = [[TestFrame alloc] init];
    frame.mainFrame = mainFrame;
    frame.request = [NSURLRequest requestWithURL:url];
    frame.securityOrigin = [[TestOrigin alloc] init];
    frame.securityOrigin.protocol = url.scheme;
    frame.securityOrigin.host = url.host;
    frame.securityOrigin.port = url.port.integerValue;
    return frame;
}

static void check(BOOL condition, NSString *message) {
    if (!condition) {
        fprintf(stderr, "%s\n", message.UTF8String);
        exit(1);
    }
}

static void checkNavigationIsolation(TestApplication *app, TestWebView *web) {
    app.serviceURL = [NSURL URLWithString:@"http://127.0.0.1:12345/"];
    app.accessToken = @"test-only-token";
    TestFrame *workspace = frame(app.serviceURL.absoluteString, YES);
    TestFrame *preview = frame(@"about:srcdoc", NO);
    TestNavigation *action = [[TestNavigation alloc] init];
    action.navigationType = WKNavigationTypeOther;
    action.sourceFrame = workspace;
    action.targetFrame = preview;
    NSArray *cases = @[
        @[@"about:srcdoc", @YES], @[@"about:blank", @YES], @[@"about:srcdoc#section", @YES],
        @[@"about:config", @NO], @[@"about:srcdoc?unexpected", @NO],
        @[@"data:text/html,hello", @NO], @[@"file:///tmp/report.html", @NO],
        @[@"http://127.0.0.1:12345/", @NO], @[@"https://example.test/", @NO],
    ];
    for (NSArray *entry in cases) {
        action.request = [NSURLRequest requestWithURL:[NSURLComponents componentsWithString:entry[0]].URL];
        __block WKNavigationActionPolicy result = WKNavigationActionPolicyCancel;
        [app webView:(WKWebView *)web decidePolicyForNavigationAction:(WKNavigationAction *)action
            decisionHandler:^(WKNavigationActionPolicy policy) { result = policy; }];
        check((result == WKNavigationActionPolicyAllow) == [entry[1] boolValue],
            [NSString stringWithFormat:@"Unexpected child navigation policy for %@", entry[0]]);
    }
    action.targetFrame = workspace;
    action.request = [NSURLRequest requestWithURL:[NSURL URLWithString:@"about:srcdoc"]];
    [app webView:(WKWebView *)web decidePolicyForNavigationAction:(WKNavigationAction *)action
        decisionHandler:^(WKNavigationActionPolicy policy) {
            check(policy == WKNavigationActionPolicyCancel, @"Inline preview replaced the main document");
        }];
    action.sourceFrame = preview;
    action.request = [NSURLRequest requestWithURL:app.serviceURL];
    web.loadedRequest = nil;
    [app webView:(WKWebView *)web decidePolicyForNavigationAction:(WKNavigationAction *)action
        decisionHandler:^(WKNavigationActionPolicy policy) {
            check(policy == WKNavigationActionPolicyCancel, @"Preview navigated the authenticated workspace");
        }];
    check(!web.loadedRequest, @"Preview navigation was replayed with workspace credentials");
    action.targetFrame = nil;
    action.navigationType = WKNavigationTypeLinkActivated;
    // Rejected paths must not inspect UI configuration or open any panels.
    NSObject *unused = [[NSObject alloc] init];
    [app webView:(WKWebView *)web createWebViewWithConfiguration:(WKWebViewConfiguration *)unused
        forNavigationAction:(WKNavigationAction *)action windowFeatures:(WKWindowFeatures *)unused];
    check(!web.loadedRequest, @"Preview popup was promoted into the workspace");
    __block BOOL panelRejected = NO;
    [app webView:(WKWebView *)web runOpenPanelWithParameters:(WKOpenPanelParameters *)unused initiatedByFrame:(WKFrameInfo *)preview
        completionHandler:^(NSArray<NSURL *> *urls) { panelRejected = urls == nil; }];
    check(panelRejected, @"Preview opened a native file picker");
    check([app isWorkspaceFrame:(WKFrameInfo *)workspace], @"Workspace origin was rejected");
    workspace.securityOrigin.port++;
    check(![app isWorkspaceFrame:(WKFrameInfo *)workspace], @"Frame request URL bypassed the security origin check");
    workspace.securityOrigin.port--;
    action.sourceFrame = workspace;
    action.targetFrame = workspace;
    [app webView:(WKWebView *)web decidePolicyForNavigationAction:(WKNavigationAction *)action
        decisionHandler:^(WKNavigationActionPolicy policy) {
            check(policy == WKNavigationActionPolicyCancel, @"Headerless workspace reload was not replayed");
        }];
    check([[web.loadedRequest valueForHTTPHeaderField:@"Authorization"] isEqual:@"Bearer test-only-token"],
        @"Workspace reload lost authentication");
    action.request = web.loadedRequest;
    [app webView:(WKWebView *)web decidePolicyForNavigationAction:(WKNavigationAction *)action
        decisionHandler:^(WKNavigationActionPolicy policy) {
            check(policy == WKNavigationActionPolicyAllow, @"Authenticated workspace reload was blocked");
        }];
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
        // ⌘W walks the workspace before it reaches the window.
        app.viewLoaded = NO;
        app.windowCloses = 0;
        content.closeRequests = 0;
        [app closeLayer:nil];
        check(app.windowCloses == 1 && content.closeRequests == 0, @"Closing without a loaded workspace did not close the window");

        app.viewLoaded = YES;
        content.layerClosed = YES;
        [app closeLayer:nil];
        check(content.closeRequests == 1 && app.windowCloses == 1, @"A panel the workspace closed still took the window with it");

        content.layerClosed = NO;
        [app closeLayer:nil];
        check(content.closeRequests == 2 && app.windowCloses == 2, @"An empty workspace did not let the window close");

        checkNavigationIsolation(app, content);
        puts("Desktop navigation recovery passed");
    }
    return 0;
}
