import XCTest
import AppKit
import SwiftUI
import Sparkle
import AlbertCore
@testable import AlbertUI

final class UpdateTests: DesktopTestCase {
    @MainActor
    func testOnlyHTTPSAndCanonicalPublicKeyEnableUpdates() {
        let key = Data(repeating: 7, count: 32).base64EncodedString()
        XCTAssertTrue(AppUpdates.validConfiguration(feed: "https://github.com/owner/repo/releases/latest/download/appcast.xml", publicKey: key))
        for url in [nil, "http://example.com/feed", "https://user:password@example.com/feed", "https://example.com/feed#fragment", "file:///tmp/feed"] as [String?] {
            XCTAssertFalse(AppUpdates.validConfiguration(feed: url, publicKey: key))
        }
        for invalid in [nil, "", "invalid", Data(repeating: 1, count: 31).base64EncodedString()] as [String?] {
            XCTAssertFalse(AppUpdates.validConfiguration(feed: "https://example.com/feed", publicKey: invalid))
        }
        let dev = AppUpdates()
        dev.start() // A test binary has no SUFeedURL; no updater/network is created.
        XCTAssertEqual(dev.phase, .unavailable)
        XCTAssertFalse(dev.canCheck)
    }

    @MainActor
    func testCheckAndDownloadedUpdateRequireSeparateExplicitInstallation() {
        var checks = 0, installs = 0, saves = 0
        let updates = AppUpdates(enabled: true, checkAction: { checks += 1 })
        updates.check(); updates.check()
        XCTAssertEqual(checks, 1)
        XCTAssertEqual(updates.phase, .checking)
        updates.receiveReady(version: "0.2.0") { choice in
            XCTAssertEqual(choice, .install)
            installs += 1
        }
        XCTAssertEqual(installs, 0)
        updates.beforeInstall = { saves += 1 }
        updates.installationAllowed = { false }
        updates.install()
        XCTAssertEqual(installs, 0)
        XCTAssertEqual(saves, 0)
        updates.installationAllowed = { true }
        updates.install(); updates.install()
        XCTAssertEqual(saves, 1)
        XCTAssertEqual(installs, 1)
        XCTAssertEqual(updates.phase, .installing)
        XCTAssertTrue(updates.installationAuthorized)
        updates.cancelForQuit() // The explicitly authorized restart is not cancelled.
        XCTAssertTrue(updates.installationAuthorized)
    }

    @MainActor
    func testFailedSaveKeepsStagedUpdateAndNeverAuthorizesInstallation() {
        let updates = AppUpdates(enabled: true)
        var choices: [SPUUserUpdateChoice] = []
        updates.receiveReady(version: "0.2.0") { choices.append($0) }
        updates.beforeInstall = { throw CocoaError(.fileWriteOutOfSpace) }
        updates.install()
        XCTAssertEqual(updates.phase, .ready)
        XCTAssertFalse(updates.installationAuthorized)
        XCTAssertTrue(choices.isEmpty)
        XCTAssertFalse(updates.message.isEmpty)
        updates.beforeInstall = {}
        updates.install()
        XCTAssertEqual(choices, [.install])
    }

    @MainActor
    func testOrdinaryQuitCancelsStagingInsteadOfConsentingToInstallOnQuit() {
        let updates = AppUpdates(enabled: true)
        var choices: [SPUUserUpdateChoice] = []
        updates.receiveReady(version: "0.2.0") { choices.append($0) }
        updates.cancelForQuit(); updates.cancelForQuit()
        XCTAssertEqual(choices, [.skip])
        XCTAssertFalse(updates.installationAuthorized)
    }

    @MainActor
    func testVerificationOrTransportFailureCannotInstallAndAcknowledgesSparkle() {
        let updates = AppUpdates(enabled: true, checkAction: {})
        var acknowledged = false, installed = false
        updates.receiveReady(version: "0.2.0") { _ in installed = true }
        updates.showUpdaterError(CocoaError(.fileReadCorruptFile)) { acknowledged = true }
        updates.install()
        XCTAssertTrue(acknowledged)
        XCTAssertFalse(installed)
        XCTAssertFalse(updates.installationAuthorized)
        XCTAssertTrue(updates.canCheck)
    }

    @MainActor
    func testInstallSavesDraftRetryAndHistoryBeforeGrantingSparkleReply() throws {
        _ = NSApplication.shared
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let updates = AppUpdates(enabled: true)
        let store = LocalStore(directory: directory)
        let model = AppModel(store: store, defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false, updates: updates)
        model.draft.text = "Неотправленный черновик"
        let request = model.draft.request()
        model.draft.taskID = "task-1"
        model.content.messages = [Message(id: "desktop:known", role: "user", text: "История")]
        var granted = false
        updates.receiveReady(version: "0.2.0") { choice in
            XCTAssertEqual(choice, .install)
            XCTAssertEqual(store.load(Draft.self, name: "draft.json")?.text, "Неотправленный черновик")
            XCTAssertEqual(store.load(Draft.self, name: "draft.json")?.pending?.clientMessageID, request.clientMessageID)
            XCTAssertEqual(store.load(Draft.self, name: "draft.json")?.taskID, "task-1")
            XCTAssertEqual(store.load(CachedContent.self, name: "cache.json")?.messages.count, 1)
            granted = true
        }
        for operation in ["send", "upload", "download", "connect", "stop", "older"] {
            model.sending = operation == "send"; model.uploading = operation == "upload"
            model.downloading = operation == "download"; model.connecting = operation == "connect"
            model.stopping = operation == "stop"; model.loadingOlder = operation == "older"
            updates.install()
            XCTAssertFalse(granted, operation)
        }
        model.loadingOlder = false
        updates.install()
        XCTAssertTrue(granted)
        XCTAssertTrue(model.modalPresented)
        XCTAssertFalse(model.canSend)
        model.escape()
        XCTAssertEqual(model.draft.text, "Неотправленный черновик")
    }

    @MainActor
    func testUpdateMenuShortcutAndSheetPreserveDraftAndRespectIME() throws {
        let app = NSApplication.shared
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let model = AppModel(store: LocalStore(directory: directory), defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false)
        model.draft.text = "Черновик"
        let controller = ApplicationController(model: model)
        controller.createWindow(show: true)
        defer { controller.window.orderOut(nil); model.shutdown() }
        controller.window.layoutIfNeeded()
        RunLoop.current.run(until: Date().addingTimeInterval(0.05))
        let item = try XCTUnwrap(controller.commands["updates"])
        XCTAssertEqual(item.keyEquivalent, "u")
        XCTAssertEqual(item.keyEquivalentModifierMask, [.command, .shift])
        let editor = try XCTUnwrap(model.composer)
        controller.window.makeFirstResponder(editor)
        editor.setMarkedText("に", selectedRange: NSRange(location: 1, length: 0), replacementRange: NSRange(location: NSNotFound, length: 0))
        controller.openUpdates()
        XCTAssertFalse(model.showUpdates)
        editor.unmarkText()
        let committedDraft = model.draft.text
        let shortcut = try XCTUnwrap(NSEvent.keyEvent(with: .keyDown, location: .zero,
            modifierFlags: [.command, .shift], timestamp: 0, windowNumber: controller.window.windowNumber,
            context: nil, characters: "U", charactersIgnoringModifiers: "u", isARepeat: false, keyCode: 32))
        XCTAssertTrue(app.mainMenu!.performKeyEquivalent(with: shortcut))
        XCTAssertTrue(model.showUpdates)
        controller.primary(); controller.toggleSidebar()
        XCTAssertFalse(model.sidebarVisible)
        XCTAssertEqual(model.draft.text, committedDraft)
        model.escape()
        XCTAssertFalse(model.showUpdates)
        XCTAssertEqual(model.draft.text, committedDraft)
        model.showSettings = true
        controller.openUpdates()
        XCTAssertFalse(model.showUpdates)
        model.showSettings = false
    }

    @MainActor
    func testUpdateSheetRendersIdleAndReadyStates() throws {
        _ = NSApplication.shared
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let updates = AppUpdates(enabled: true, currentVersion: "0.1.0", checkAction: {})
        let model = AppModel(store: LocalStore(directory: directory), defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false, updates: updates)
        for name in ["idle", "ready"] {
            if name == "ready" { updates.receiveReady(version: "0.2.0") { _ in } }
            let host = NSHostingView(rootView: UpdatesView(updates: updates, model: model))
            let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 430, height: 220), styleMask: [.titled], backing: .buffered, defer: false)
            window.contentView = host
            window.setContentSize(host.fittingSize)
            window.orderFront(nil)
            defer { window.orderOut(nil) }
            window.layoutIfNeeded()
            RunLoop.current.run(until: Date().addingTimeInterval(0.05))
            XCTAssertGreaterThan(host.bounds.height, 120)
            XCTAssertLessThan(host.bounds.height, 400)
            let bitmap = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
            host.cacheDisplay(in: host.bounds, to: bitmap)
            let png = try XCTUnwrap(bitmap.representation(using: .png, properties: [:]))
            XCTAssertGreaterThan(png.count, 100)
            if let path = ProcessInfo.processInfo.environment["ALBERT_UPDATE_SNAPSHOT_DIR"] {
                let output = URL(fileURLWithPath: path, isDirectory: true)
                try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
                try png.write(to: output.appendingPathComponent("updates-\(name).png"))
            }
        }
    }

    @MainActor
    func testActualUpdateWindowKeyboardCheckAndInstallShareBusyGuards() throws {
        _ = NSApplication.shared
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        var checks = 0, installed = 0
        let updates = AppUpdates(enabled: true, currentVersion: "0.1.0", checkAction: { checks += 1 })
        let model = AppModel(store: LocalStore(directory: directory), defaults: UserDefaults(suiteName: UUID().uuidString)!, startNetwork: false, updates: updates)
        model.showUpdates = true
        model.draft.text = "Черновик перед обновлением"
        let host = NSHostingView(rootView: UpdatesView(updates: updates, model: model))
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 430, height: 220), styleMask: [.titled], backing: .buffered, defer: false)
        window.contentView = host; window.makeKeyAndOrderFront(nil)
        defer { window.orderOut(nil); model.shutdown() }
        RunLoop.current.run(until: Date().addingTimeInterval(0.1))
        func event(_ code: UInt16, _ characters: String) -> NSEvent {
            KeyboardSmoke.event(code: code, characters: characters, window: window)
        }
        _ = window.performKeyEquivalent(with: event(15, "r"))
        _ = window.performKeyEquivalent(with: event(15, "r"))
        XCTAssertEqual(checks, 1)
        updates.receiveReady(version: "0.2.0") { _ in installed += 1 }
        model.sending = true
        RunLoop.current.run(until: Date().addingTimeInterval(0.05))
        _ = window.performKeyEquivalent(with: event(36, "\r"))
        XCTAssertEqual(installed, 0)
        model.sending = false
        RunLoop.current.run(until: Date().addingTimeInterval(0.05))
        _ = window.performKeyEquivalent(with: event(36, "\r"))
        _ = window.performKeyEquivalent(with: event(36, "\r"))
        XCTAssertEqual(installed, 1)
        XCTAssertEqual(model.store.load(Draft.self, name: "draft.json")?.text, "Черновик перед обновлением")
    }
}
