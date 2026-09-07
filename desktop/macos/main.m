#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

@interface SteveApplication : NSObject <NSApplicationDelegate, WKNavigationDelegate, WKUIDelegate>
@property(nonatomic, strong) NSWindow *window;
@property(nonatomic, strong) WKWebView *webView;
@property(nonatomic, strong) NSURL *serviceURL;
@property(nonatomic, strong) NSString *accessToken;
@property(nonatomic, assign) BOOL launching;
@property(nonatomic, assign) BOOL viewLoaded;
@end

@implementation SteveApplication

- (void)applicationDidFinishLaunching:(NSNotification *)notification {
    [self installMenus];
    [self showWindow];
    [self connectService];
}

- (void)installMenus {
    NSMenu *bar = [[NSMenu alloc] init];
    NSMenuItem *app = [[NSMenuItem alloc] init];
    [bar addItem:app];
    NSMenu *appMenu = [[NSMenu alloc] initWithTitle:@"Steve"];
    [appMenu addItemWithTitle:@"关于 Steve" action:@selector(orderFrontStandardAboutPanel:) keyEquivalent:@""];
    [appMenu addItem:[NSMenuItem separatorItem]];
    [appMenu addItemWithTitle:@"隐藏 Steve" action:@selector(hide:) keyEquivalent:@"h"];
    [appMenu addItem:[NSMenuItem separatorItem]];
    [appMenu addItemWithTitle:@"退出 Steve" action:@selector(terminate:) keyEquivalent:@"q"];
    app.submenu = appMenu;

    NSMenuItem *edit = [[NSMenuItem alloc] init];
    [bar addItem:edit];
    NSMenu *editMenu = [[NSMenu alloc] initWithTitle:@"编辑"];
    [editMenu addItemWithTitle:@"撤销" action:@selector(undo:) keyEquivalent:@"z"];
    NSMenuItem *redo = [editMenu addItemWithTitle:@"重做" action:@selector(redo:) keyEquivalent:@"Z"];
    redo.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagShift;
    [editMenu addItem:[NSMenuItem separatorItem]];
    [editMenu addItemWithTitle:@"剪切" action:@selector(cut:) keyEquivalent:@"x"];
    [editMenu addItemWithTitle:@"复制" action:@selector(copy:) keyEquivalent:@"c"];
    [editMenu addItemWithTitle:@"粘贴" action:@selector(paste:) keyEquivalent:@"v"];
    [editMenu addItemWithTitle:@"全选" action:@selector(selectAll:) keyEquivalent:@"a"];
    edit.submenu = editMenu;

    NSMenuItem *windowItem = [[NSMenuItem alloc] init];
    [bar addItem:windowItem];
    NSMenu *windowMenu = [[NSMenu alloc] initWithTitle:@"窗口"];
    NSMenuItem *show = [windowMenu addItemWithTitle:@"打开工作台" action:@selector(openWorkspace:) keyEquivalent:@"0"];
    show.target = self;
    [windowMenu addItemWithTitle:@"最小化" action:@selector(performMiniaturize:) keyEquivalent:@"m"];
    [windowMenu addItemWithTitle:@"关闭窗口" action:@selector(performClose:) keyEquivalent:@"w"];
    windowItem.submenu = windowMenu;
    NSApp.windowsMenu = windowMenu;
    NSApp.mainMenu = bar;
}

- (void)showWindow {
    if (!self.window) {
        self.window = [[NSWindow alloc] initWithContentRect:NSMakeRect(0, 0, 1240, 820)
            styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable
            backing:NSBackingStoreBuffered defer:NO];
        self.window.title = @"Steve";
        self.window.minSize = NSMakeSize(780, 540);
        self.window.releasedWhenClosed = NO;
        [self.window setFrameAutosaveName:@"SteveWorkspace"];
        [self.window center];
    }
    [self.window makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
}

- (void)openWorkspace:(id)sender {
    [self showWindow];
    [self connectService];
}

- (BOOL)applicationShouldHandleReopen:(NSApplication *)sender hasVisibleWindows:(BOOL)flag {
    [self openWorkspace:nil];
    return YES;
}

- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)sender { return NO; }

- (void)showStarting {
    NSView *view = [[NSView alloc] initWithFrame:self.window.contentView.bounds];
    view.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
    NSTextField *label = [NSTextField labelWithString:@"正在连接本机工作台…"];
    label.font = [NSFont systemFontOfSize:16 weight:NSFontWeightMedium];
    label.textColor = NSColor.secondaryLabelColor;
    label.translatesAutoresizingMaskIntoConstraints = NO;
    [view addSubview:label];
    [NSLayoutConstraint activateConstraints:@[
        [label.centerXAnchor constraintEqualToAnchor:view.centerXAnchor],
        [label.centerYAnchor constraintEqualToAnchor:view.centerYAnchor]
    ]];
    self.window.contentView = view;
}

- (void)connectService {
    if (self.launching) return;
    self.launching = YES;
    [self showStarting];
    NSString *binary = [NSBundle.mainBundle pathForResource:@"steve" ofType:nil];
    NSString *stateDir = NSProcessInfo.processInfo.environment[@"STEVE_DESKTOP_STATE_DIR"];
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
        NSTask *task = [[NSTask alloc] init];
        task.executableURL = [NSURL fileURLWithPath:binary ?: @""];
        NSMutableArray<NSString *> *arguments = [NSMutableArray arrayWithArray:@[@"desktop", @"--json"]];
        if (stateDir.length) [arguments addObjectsFromArray:@[@"--state-dir", stateDir]];
        task.arguments = arguments;
        NSPipe *output = [NSPipe pipe];
        NSPipe *errors = [NSPipe pipe];
        task.standardOutput = output;
        task.standardError = errors;
        NSError *failure = nil;
        NSDictionary *result = nil;
        NSString *detail = nil;
        if ([task launchAndReturnError:&failure]) {
            NSData *raw = [output.fileHandleForReading readDataToEndOfFile];
            NSData *errorData = [errors.fileHandleForReading readDataToEndOfFile];
            [task waitUntilExit];
            if (task.terminationStatus == 0) {
                id decoded = [NSJSONSerialization JSONObjectWithData:raw options:0 error:&failure];
                if ([decoded isKindOfClass:NSDictionary.class]) result = decoded;
            } else {
                detail = [[NSString alloc] initWithData:errorData encoding:NSUTF8StringEncoding];
            }
        }
        dispatch_async(dispatch_get_main_queue(), ^{
            self.launching = NO;
            NSString *address = [result[@"authenticated_url"] isKindOfClass:NSString.class] ? result[@"authenticated_url"] : nil;
            if (!address || ![self openService:address]) {
                [self showConnectionFailure:detail ?: failure.localizedDescription ?: @"本机服务未返回可用的工作台地址。"];
            }
        });
    });
}

- (BOOL)openService:(NSString *)address {
    NSURLComponents *parts = [NSURLComponents componentsWithString:address];
    if (![parts.scheme isEqualToString:@"http"] ||
        (!([parts.host isEqualToString:@"127.0.0.1"] || [parts.host isEqualToString:@"::1"] || [parts.host isEqualToString:@"[::1]"])) ||
        !parts.port || parts.port.integerValue < 1 || parts.user || parts.password) return NO;
    NSString *token = nil;
    for (NSURLQueryItem *item in parts.queryItems) if ([item.name isEqualToString:@"token"]) token = item.value;
    if (token.length < 40) return NO;
    parts.queryItems = nil;
    parts.fragment = nil;
    if (self.webView && self.viewLoaded && [self isServiceURL:parts.URL] && [self.accessToken isEqualToString:token]) {
        // Reopening a closed window also checks the service, while retaining
        // the current conversation and any unsent editor content in the view.
        self.webView.frame = self.window.contentView.bounds;
        self.window.contentView = self.webView;
        return YES;
    }
    self.serviceURL = parts.URL;
    self.accessToken = token;

    WKWebViewConfiguration *configuration = [[WKWebViewConfiguration alloc] init];
    configuration.websiteDataStore = WKWebsiteDataStore.defaultDataStore;
    NSData *serialized = [NSJSONSerialization dataWithJSONObject:@[token] options:0 error:nil];
    NSString *literal = [[NSString alloc] initWithData:serialized encoding:NSUTF8StringEncoding];
    NSString *script = [NSString stringWithFormat:@"sessionStorage.setItem('steve.token', (%@)[0]);", literal];
    [configuration.userContentController addUserScript:[[WKUserScript alloc] initWithSource:script injectionTime:WKUserScriptInjectionTimeAtDocumentStart forMainFrameOnly:YES]];
    self.webView = [[WKWebView alloc] initWithFrame:self.window.contentView.bounds configuration:configuration];
    self.webView.navigationDelegate = self;
    self.webView.UIDelegate = self;
    self.webView.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
    self.webView.allowsBackForwardNavigationGestures = NO;
    self.viewLoaded = NO;
    self.window.contentView = self.webView;
    NSMutableURLRequest *request = [NSMutableURLRequest requestWithURL:self.serviceURL];
    [request setValue:[@"Bearer " stringByAppendingString:token] forHTTPHeaderField:@"Authorization"];
    [self.webView loadRequest:request];
    return YES;
}

- (void)showConnectionFailure:(NSString *)detail {
    NSAlert *alert = [[NSAlert alloc] init];
    alert.messageText = @"暂时无法连接本机服务";
    alert.informativeText = detail;
    [alert addButtonWithTitle:@"重试"];
    [alert addButtonWithTitle:@"关闭窗口"];
    [alert beginSheetModalForWindow:self.window completionHandler:^(NSModalResponse response) {
        if (response == NSAlertFirstButtonReturn) [self connectService];
        else [self.window close];
    }];
}

- (BOOL)isServiceURL:(NSURL *)url {
    return [url.scheme isEqualToString:self.serviceURL.scheme] &&
        [url.host isEqualToString:self.serviceURL.host] && [url.port isEqualToNumber:self.serviceURL.port];
}

- (void)webView:(WKWebView *)webView decidePolicyForNavigationAction:(WKNavigationAction *)action decisionHandler:(void (^)(WKNavigationActionPolicy))decisionHandler {
    NSURL *url = action.request.URL;
    if ([self isServiceURL:url]) {
        // Main-document navigation does not inherit the initial request's
        // header. Keep reloads authenticated without putting tokens in URLs.
        if (action.targetFrame.isMainFrame && ![action.request valueForHTTPHeaderField:@"Authorization"].length) {
            NSMutableURLRequest *request = [action.request mutableCopy];
            [request setValue:[@"Bearer " stringByAppendingString:self.accessToken] forHTTPHeaderField:@"Authorization"];
            decisionHandler(WKNavigationActionPolicyCancel);
            [webView loadRequest:request];
            return;
        }
        decisionHandler(WKNavigationActionPolicyAllow);
        return;
    }
    if (action.navigationType == WKNavigationTypeLinkActivated &&
        ([url.scheme isEqualToString:@"https"] || [url.scheme isEqualToString:@"http"])) {
        [NSWorkspace.sharedWorkspace openURL:url];
    }
    decisionHandler(WKNavigationActionPolicyCancel);
}

- (WKWebView *)webView:(WKWebView *)webView createWebViewWithConfiguration:(WKWebViewConfiguration *)configuration forNavigationAction:(WKNavigationAction *)action windowFeatures:(WKWindowFeatures *)windowFeatures {
    if (!action.targetFrame && action.navigationType == WKNavigationTypeLinkActivated) {
        if ([self isServiceURL:action.request.URL]) [webView loadRequest:action.request];
        else if ([action.request.URL.scheme isEqualToString:@"https"] || [action.request.URL.scheme isEqualToString:@"http"]) [NSWorkspace.sharedWorkspace openURL:action.request.URL];
    }
    return nil;
}

- (void)webView:(WKWebView *)webView runOpenPanelWithParameters:(WKOpenPanelParameters *)parameters initiatedByFrame:(WKFrameInfo *)frame completionHandler:(void (^)(NSArray<NSURL *> *))completionHandler {
    NSOpenPanel *panel = [NSOpenPanel openPanel];
    panel.canChooseFiles = YES;
    panel.canChooseDirectories = parameters.allowsDirectories;
    panel.allowsMultipleSelection = parameters.allowsMultipleSelection;
    [panel beginSheetModalForWindow:self.window completionHandler:^(NSModalResponse response) {
        completionHandler(response == NSModalResponseOK ? panel.URLs : nil);
    }];
}

- (void)webView:(WKWebView *)webView didFailProvisionalNavigation:(WKNavigation *)navigation withError:(NSError *)error {
    self.viewLoaded = NO;
    if (error.code != NSURLErrorCancelled) [self showConnectionFailure:@"本机服务暂时不可用。重试会连接现有服务或重新启动服务，任务进度保存在本机。"];
}

- (void)webView:(WKWebView *)webView didFinishNavigation:(WKNavigation *)navigation { self.viewLoaded = YES; }

@end

int main(int argc, const char *argv[]) {
    @autoreleasepool {
        NSApplication *app = [NSApplication sharedApplication];
        app.activationPolicy = NSApplicationActivationPolicyRegular;
        SteveApplication *delegate = [[SteveApplication alloc] init];
        app.delegate = delegate;
        [app run];
    }
    return 0;
}
