#define main steveApplicationMain
#include "main.m"
#undef main

@interface TestApplication : SteveApplication
@property(nonatomic, assign) NSUInteger failures;
@property(nonatomic, assign) NSUInteger connections;
@property(nonatomic, assign) NSUInteger windowCloses;
@property(nonatomic, copy) NSString *launcherAddress;
@property(nonatomic, assign) NSUInteger launches;
@property(nonatomic, assign) NSUInteger opens;
@property(nonatomic, assign) BOOL holdLauncher;
@property(nonatomic, copy) void (^heldLauncher)(NSString *, NSString *);
@property(nonatomic, assign) NSUInteger confirmations;
@property(nonatomic, copy) void (^heldConfirmation)(BOOL);
- (void)presentJavaScriptConfirmation:(NSString *)message window:(NSWindow *)window completion:(void (^)(BOOL))completion;

@end

@implementation TestApplication
- (void)presentJavaScriptConfirmation:(NSString *)message window:(NSWindow *)window completion:(void (^)(BOOL))completion {
    self.confirmations++;
    self.heldConfirmation = completion;
}
- (void)runLauncher:(void (^)(NSString *, NSString *))completion {
    self.launches++;
    if (self.holdLauncher) { self.heldLauncher = completion; return; }
    completion(self.launcherAddress, self.launcherAddress ? nil : @"launcher failed");
}
- (BOOL)openService:(NSString *)address { self.opens++; return YES; }
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
/** The document the view shows, as WKWebView.URL reports it. */
@property(nonatomic, strong) NSURL *URL;
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
    action.targetFrame = nil;
    action.request = [NSURLRequest requestWithURL:app.serviceURL];
    web.loadedRequest = nil;
    [app webView:(WKWebView *)web decidePolicyForNavigationAction:(WKNavigationAction *)action
        decisionHandler:^(WKNavigationActionPolicy policy) {
            check(policy == WKNavigationActionPolicyAllow, @"Trusted service link did not reach the UI delegate");
        }];
    [app webView:(WKWebView *)web createWebViewWithConfiguration:(WKWebViewConfiguration *)unused
        forNavigationAction:(WKNavigationAction *)action windowFeatures:(WKWindowFeatures *)unused];
    check([web.loadedRequest.URL isEqual:app.serviceURL], @"Trusted service link was not kept in the workspace");
}

@interface TestWindow : NSObject
@property(nonatomic, assign, getter=isVisible) BOOL visible;
@property(nonatomic, strong) id attachedSheet;
@end
@implementation TestWindow
@end

static void checkJavaScriptConfirmIsolation(void) {
    SEL selector = @selector(webView:runJavaScriptConfirmPanelWithMessage:initiatedByFrame:completionHandler:);
    TestApplication *app = [[TestApplication alloc] init];
    check([app respondsToSelector:selector], @"Workspace window.confirm defaults to rejection without a native delegate");
    TestWebView *web = [[TestWebView alloc] init];
    TestWindow *window = [[TestWindow alloc] init];
    window.visible = YES;
    app.window = (NSWindow *)window;
    app.serviceURL = [NSURL URLWithString:@"http://127.0.0.1:12345/"];
    web.URL = app.serviceURL;
    app.webView = (WKWebView *)web;
    app.viewLoaded = YES;
    TestFrame *workspace = frame(app.serviceURL.absoluteString, YES);
    TestFrame *preview = frame(@"about:srcdoc", NO);
    __block NSUInteger calls = 0;
    __block BOOL result = NO;
    void (^answered)(BOOL) = ^(BOOL accepted) { calls++; result = accepted; };
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"trusted"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    check(app.confirmations == 1 && calls == 0, @"Trusted confirm did not wait for a native decision");
    void (^firstAnswer)(BOOL) = app.heldConfirmation;
    firstAnswer(YES);
    check(calls == 1 && result, @"Accepted native confirm did not answer true");
    firstAnswer(NO);
    check(calls == 1, @"A confirmation was answered twice");
    calls = 0;
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"cancel"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    app.heldConfirmation(NO);
    check(calls == 1 && !result, @"Cancelled native confirm did not answer false");
    NSUInteger shown = app.confirmations;
    NSArray *rejected = @[preview, frame(app.serviceURL.absoluteString, NO),
        frame(@"http://127.0.0.1:23456/", YES), frame(@"https://example.test/", YES)];
    for (TestFrame *untrusted in rejected) {
        calls = 0; result = YES;
        [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"untrusted"
            initiatedByFrame:(WKFrameInfo *)untrusted completionHandler:answered];
        check(calls == 1 && !result && app.confirmations == shown, @"Untrusted frame opened a native confirm");
    }
    workspace.securityOrigin.port++;
    calls = 0; result = YES;
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"origin mismatch"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    check(calls == 1 && !result && app.confirmations == shown, @"Frame URL bypassed origin checks for confirm");
    workspace.securityOrigin.port--;
    calls = 0; result = YES;
    [app webView:(WKWebView *)[[TestWebView alloc] init] runJavaScriptConfirmPanelWithMessage:@"stale view"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    check(calls == 1 && !result && app.confirmations == shown, @"Stale view opened a native confirm");
    window.attachedSheet = [[NSObject alloc] init];
    calls = 0; result = YES;
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"sheet busy"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    check(calls == 1 && !result && app.confirmations == shown, @"An existing sheet queued another confirm");
    window.attachedSheet = nil;
    calls = 0;
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"pending"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    void (^pending)(BOOL) = app.heldConfirmation;
    NSUInteger pendingShown = app.confirmations;
    __block BOOL repeated = YES;
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"second pending"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:^(BOOL accepted) { repeated = accepted; }];
    check(!repeated && app.confirmations == pendingShown, @"A pending confirm admitted another confirm");
    app.loadGeneration++;
    pending(YES);
    check(calls == 1 && !result, @"A decision from before navigation approved the new document");
    calls = 0;
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"replace"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    pending = app.heldConfirmation;
    app.webView = (WKWebView *)[[TestWebView alloc] init];
    pending(YES);
    check(calls == 1 && !result, @"A retired view received a positive confirm");
    app.webView = (WKWebView *)web;
    calls = 0;
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"hide"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    pending = app.heldConfirmation;
    window.visible = NO;
    pending(YES);
    check(calls == 1 && !result, @"A closed window received a positive confirm");
    window.visible = YES;
    calls = 0;
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"window replacement"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    pending = app.heldConfirmation;
    app.window = (NSWindow *)[[TestWindow alloc] init];
    pending(YES);
    check(calls == 1 && !result, @"A replacement window received an old decision");
    app.window = (NSWindow *)window;
    calls = 0;
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"frame origin changed"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    pending = app.heldConfirmation;
    workspace.securityOrigin.port++;
    pending(YES);
    check(calls == 1 && !result, @"A changed frame origin received a positive decision");
    workspace.securityOrigin.port--;
    app.viewLoaded = NO;
    shown = app.confirmations;
    calls = 0; result = YES;
    [app webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"not loaded"
        initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
    check(calls == 1 && !result && app.confirmations == shown, @"An unloaded workspace opened a confirm");
    calls = 0; result = YES;
    void (^orphanedAnswer)(BOOL) = nil;
    __weak TestApplication *weakOwner = nil;
    @autoreleasepool {
        TestApplication *disposable = [[TestApplication alloc] init];
        disposable.window = (NSWindow *)window;
        disposable.webView = (WKWebView *)web;
        disposable.serviceURL = app.serviceURL;
        disposable.viewLoaded = YES;
        weakOwner = disposable;
        [disposable webView:(WKWebView *)web runJavaScriptConfirmPanelWithMessage:@"orphaned"
            initiatedByFrame:(WKFrameInfo *)workspace completionHandler:answered];
        orphanedAnswer = disposable.heldConfirmation;
    }
    check(!weakOwner, @"A pending confirmation retained the application owner");
    orphanedAnswer(YES);
    check(calls == 1 && !result, @"A lost owner left a confirmation unanswered or approved");
    puts("Desktop confirm ownership passed");
}

// The console's sidebar links are fragment routes (#/tasks). WebKit reports
// an empty source frame for them, so they must not depend on it.
static void checkFragmentRoutes(TestApplication *app, TestWebView *web) {
    TestFrame *unknown = [[TestFrame alloc] init];
    unknown.mainFrame = YES;
    unknown.request = [[NSURLRequest alloc] init];
    unknown.securityOrigin = [[TestOrigin alloc] init];
    TestNavigation *action = [[TestNavigation alloc] init];
    action.navigationType = WKNavigationTypeLinkActivated;
    action.sourceFrame = unknown;
    action.targetFrame = unknown;
    NSArray *cases = @[
        @[@"http://127.0.0.1:12345/", @"http://127.0.0.1:12345/#/tasks", @YES],
        @[@"http://127.0.0.1:12345/#/home", @"http://127.0.0.1:12345/#/console?view=board", @YES],
        @[@"http://127.0.0.1:12345/#/home", @"http://127.0.0.1:12345/", @NO],
        @[@"http://127.0.0.1:12345/", @"http://127.0.0.1:12345/other#/tasks", @NO],
        @[@"about:blank", @"http://127.0.0.1:12345/#/tasks", @NO],
    ];
    for (NSArray *entry in cases) {
        web.URL = [NSURL URLWithString:entry[0]];
        action.request = [NSURLRequest requestWithURL:[NSURL URLWithString:entry[1]]];
        web.loadedRequest = nil;
        __block WKNavigationActionPolicy result = WKNavigationActionPolicyCancel;
        [app webView:(WKWebView *)web decidePolicyForNavigationAction:(WKNavigationAction *)action
            decisionHandler:^(WKNavigationActionPolicy policy) { result = policy; }];
        check((result == WKNavigationActionPolicyAllow) == [entry[2] boolValue],
            [NSString stringWithFormat:@"Unexpected fragment route policy from %@ to %@", entry[0], entry[1]]);
        check(!web.loadedRequest, @"A fragment route was replayed with workspace credentials");
    }
    web.URL = nil;
}

// The page asks the shell to make sure the service runs while its live
// connection is down. A service back at the same address keeps the view;
// a new address replaces it; a failed check never covers the workspace.
static void checkServiceWatch(void) {
    TestApplication *app = [[TestApplication alloc] init];
    NSString *token = [@"" stringByPaddingToLength:48 withString:@"a" startingAtIndex:0];
    app.serviceURL = [NSURL URLWithString:@"http://127.0.0.1:4100"];
    app.accessToken = token;
    app.launcherAddress = [NSString stringWithFormat:@"http://127.0.0.1:4100?token=%@", token];
    [app ensureService];
    check(app.launches == 1 && app.opens == 0 && app.failures == 0 && !app.launching, @"A service back at the same address replaced the workspace");
    app.launcherAddress = [NSString stringWithFormat:@"http://127.0.0.1:4200?token=%@", token];
    [app ensureService];
    check(app.opens == 1, @"A service at a new address was not opened");
    app.launcherAddress = nil;
    [app ensureService];
    check(app.failures == 0 && !app.launching, @"A failed service check covered the workspace");
    app.holdLauncher = YES;
    [app ensureService];
    [app ensureService];
    check(app.launches == 4, @"Service checks were not serialized");
    app.heldLauncher(nil, @"still starting");
    check(!app.launching, @"A finished service check stayed in progress");
    TestApplication *fresh = [[TestApplication alloc] init];
    [fresh ensureService];
    check(fresh.launches == 0, @"A service check ran before any workspace was opened");
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

        checkJavaScriptConfirmIsolation();
        checkNavigationIsolation(app, content);
        checkFragmentRoutes(app, content);
        checkServiceWatch();
        puts("Desktop navigation recovery passed");
    }
    return 0;
}
