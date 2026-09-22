#define main steveApplicationMain
#include "main.m"
#undef main

@interface PreviewApplication : SteveApplication
@property(nonatomic, assign) NSUInteger directoryAnswers;
@property(nonatomic, assign) NSUInteger previewMessages;
@end
@implementation PreviewApplication
- (WKWebViewConfiguration *)workspaceConfiguration {
    WKWebViewConfiguration *configuration = [super workspaceConfiguration];
    configuration.websiteDataStore = WKWebsiteDataStore.nonPersistentDataStore;
    return configuration;
}
- (void)answerDirectoryRequest:(NSString *)identifier path:(id)path { self.directoryAnswers++; }
- (void)userContentController:(WKUserContentController *)controller didReceiveScriptMessage:(WKScriptMessage *)message {
    if (!message.frameInfo.isMainFrame) self.previewMessages++;
    [super userContentController:controller didReceiveScriptMessage:message];
}
@end

static void require(BOOL condition, NSString *message) {
    if (!condition) {
        fprintf(stderr, "%s\n", message.UTF8String);
        exit(1);
    }
}

static id evaluate(WKWebView *web, NSString *script) {
    __block BOOL done = NO;
    __block id value = nil;
    [web evaluateJavaScript:script completionHandler:^(id result, NSError *error) {
        require(!error, error.localizedDescription);
        value = result;
        done = YES;
    }];
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:5];
    while (!done && deadline.timeIntervalSinceNow > 0)
        [NSRunLoop.currentRunLoop runUntilDate:[NSDate dateWithTimeIntervalSinceNow:0.01]];
    require(done, @"JavaScript evaluation timed out");
    return value;
}

static void waitFor(WKWebView *web, NSString *expression, NSString *message) {
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:10];
    while (deadline.timeIntervalSinceNow > 0) {
        if ([evaluate(web, expression) boolValue]) return;
        [NSRunLoop.currentRunLoop runUntilDate:[NSDate dateWithTimeIntervalSinceNow:0.02]];
    }
    require(NO, message);
}

// Run against a disposable loopback fixture, never the installed service.
int main(int argc, const char *argv[]) {
    @autoreleasepool {
        require(argc == 2, @"Expected the isolated fixture URL");
        [NSApplication sharedApplication];
        NSApp.activationPolicy = NSApplicationActivationPolicyProhibited;
        PreviewApplication *app = [[PreviewApplication alloc] init];
        // Valid main-frame requests return "cancelled" without opening a
        // native sheet; malicious child messages must not reach that path.
        app.pickingDirectory = YES;
        app.window = [[NSWindow alloc] initWithContentRect:NSMakeRect(0, 0, 900, 700)
            styleMask:NSWindowStyleMaskBorderless backing:NSBackingStoreBuffered defer:NO];
        require([app openService:[NSString stringWithUTF8String:argv[1]]], @"Fixture URL rejected");
        waitFor(app.webView, @"Boolean(window.previewResult)", @"Sandboxed srcdoc was cancelled: HTML preview stayed blank");
        NSDictionary *result = evaluate(app.webView, @"window.previewResult");
        require([result[@"text"] isEqual:@"Preview report"], @"Static HTML did not render");
        require([result[@"styled"] boolValue], @"Preview CSS did not render");
        require([result[@"parentBlocked"] boolValue], @"Preview can read the parent document");
        require([result[@"storageBlocked"] boolValue], @"Preview can access session storage");
        require([result[@"bridgeAbsent"] boolValue], @"Privileged desktop script was injected into the preview");
        require([result[@"nativeMessageSent"] boolValue], @"Fixture did not probe the native message handler");
        require(app.previewMessages == 1, @"Native bridge isolation was not exercised");
        require(app.directoryAnswers == 0, @"Preview invoked a privileged native action");
        require([evaluate(app.webView, @"document.getElementById('root').textContent === 'Workspace'") boolValue],
            @"Preview modified the workspace");
        require([evaluate(app.webView, @"getComputedStyle(document.body).color === 'rgb(1, 2, 3)'") boolValue],
            @"Preview CSS escaped its container");
        evaluate(app.webView, @"window.steveDesktop.pickDirectory(''); true");
        NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:5];
        while (!app.directoryAnswers && deadline.timeIntervalSinceNow > 0)
            [NSRunLoop.currentRunLoop runUntilDate:[NSDate dateWithTimeIntervalSinceNow:0.01]];
        require(app.directoryAnswers == 1, @"Trusted workspace lost its native bridge");
        // Exercise the real headerless navigation path, not just a mocked action.
        evaluate(app.webView, @"window.previewResult = null; location.reload(); true");
        waitFor(app.webView, @"Boolean(window.previewResult)", @"Reload lost the authenticated workspace or preview");
        require(app.directoryAnswers == 1, @"Reloaded preview invoked a privileged native action");
        puts("Desktop sandboxed HTML preview passed");
        [app.webView.configuration.userContentController removeScriptMessageHandlerForName:@"steve"];
        [app.webView stopLoading];
        app.webView.navigationDelegate = nil;
        app.webView.UIDelegate = nil;
    }
    return 0;
}
