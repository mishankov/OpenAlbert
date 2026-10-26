import AppKit
import SwiftUI
import Sparkle

/// Sparkle owns transport, signature verification and replacement. This driver
/// grants installation only after an explicit command and durable draft save.
@MainActor
public final class AppUpdates: NSObject, ObservableObject, SPUUserDriver, SPUUpdaterDelegate {
    public enum Phase: Equatable { case unavailable, idle, checking, current, downloading, ready, installing, error }
    @Published public private(set) var phase: Phase
    @Published public private(set) var version = ""
    @Published public private(set) var progress = 0.0
    @Published public private(set) var message = ""
    public private(set) var installationAuthorized = false
    public var installationAllowed: () -> Bool = { true }
    public var beforeInstall: () throws -> Void = {}
    public var restoreAfterFailure: () -> Void = {}
    private var updater: SPUUpdater?
    private var readyReply: ((SPUUserUpdateChoice) -> Void)?
    private var cancellation: (() -> Void)?
    private let checkAction: (() -> Void)?
    private let versionOverride: String?
    private var received: UInt64 = 0
    private var expected: UInt64 = 0
    public var currentVersion: String {
        versionOverride ?? Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"
    }

    public init(enabled: Bool = false, currentVersion: String? = nil, checkAction: (() -> Void)? = nil) {
        phase = enabled ? .idle : .unavailable
        versionOverride = currentVersion
        self.checkAction = checkAction
        super.init()
    }
    public static func validConfiguration(feed: String?, publicKey: String?) -> Bool {
        guard let feed, let parts = URLComponents(string: feed), parts.scheme == "https",
              let host = parts.host, !host.isEmpty, parts.user == nil, parts.password == nil,
              parts.fragment == nil, let publicKey, let bytes = Data(base64Encoded: publicKey),
              bytes.count == 32, bytes.base64EncodedString() == publicKey else { return false }
        return true
    }
    public func start() {
        guard updater == nil,
              Self.validConfiguration(feed: Bundle.main.object(forInfoDictionaryKey: "SUFeedURL") as? String,
                                      publicKey: Bundle.main.object(forInfoDictionaryKey: "SUPublicEDKey") as? String) else { return }
        let updater = SPUUpdater(hostBundle: .main, applicationBundle: .main, userDriver: self, delegate: self)
        self.updater = updater
        do {
            try updater.start()
            updater.clearFeedURLFromUserDefaults()
            updater.automaticallyChecksForUpdates = true
            updater.automaticallyDownloadsUpdates = false
            updater.sendsSystemProfile = false
            phase = .idle
            updater.checkForUpdatesInBackground()
        } catch { receiveError() }
    }
    public var canCheck: Bool {
        [.idle, .current, .error].contains(phase) && (checkAction != nil || updater?.canCheckForUpdates == true)
    }
    public var canInstall: Bool { phase == .ready && readyReply != nil && installationAllowed() }
    public func check() {
        guard canCheck, KeyboardGate.permitsPrimaryAction() else { return }
        phase = .checking; message = ""; progress = 0
        if let checkAction { checkAction() } else { updater?.checkForUpdates() }
    }
    public func install() {
        guard canInstall, KeyboardGate.permitsPrimaryAction(), let reply = readyReply else { return }
        do {
            try beforeInstall()
            readyReply = nil; cancellation = nil
            installationAuthorized = true; phase = .installing; message = ""
            reply(.install)
        } catch {
            restoreAfterFailure()
            message = "Не удалось сохранить черновик и историю. Повторите установку."
        }
    }
    public func cancelForQuit() {
        guard !installationAuthorized else { return }
        let reply = readyReply, cancel = cancellation
        readyReply = nil; cancellation = nil
        // Dismiss permits installation on quit; skip explicitly cancels staging.
        if let reply { reply(.skip) } else { cancel?() }
    }
    func receiveReady(version: String, reply: @escaping (SPUUserUpdateChoice) -> Void) {
        guard phase != .unavailable, phase != .installing else { reply(.skip); return }
        self.version = version; readyReply = reply; cancellation = nil
        progress = 1; phase = .ready; message = ""
    }
    func receiveError() {
        if phase == .installing { restoreAfterFailure() }
        installationAuthorized = false; readyReply = nil; cancellation = nil
        phase = .error; message = "Не удалось обновить приложение. Повторите проверку."
    }
    public func updater(_ updater: SPUUpdater, mayPerform updateCheck: SPUUpdateCheck) throws {
        if phase == .installing { throw NSError(domain: "AlbertUpdates", code: 1) }
        if [.idle, .current, .error].contains(phase) { phase = .checking }
    }
    public func updater(_ updater: SPUUpdater, didFinishUpdateCycleFor updateCheck: SPUUpdateCheck, error: Error?) {
        guard phase == .checking else { return }
        let failure = error as NSError?
        if failure == nil || (failure?.domain == SUSparkleErrorDomain && failure?.code == Int(SUError.noUpdateError.rawValue)) {
            phase = .current
        } else { receiveError() }
    }
    public func show(_ request: SPUUpdatePermissionRequest, reply: @escaping (SUUpdatePermissionResponse) -> Void) {
        reply(SUUpdatePermissionResponse(automaticUpdateChecks: true, sendSystemProfile: false))
    }
    public func showUserInitiatedUpdateCheck(cancellation: @escaping () -> Void) {
        self.cancellation = cancellation; phase = .checking
    }
    public func showUpdateFound(with appcastItem: SUAppcastItem, state: SPUUserUpdateState, reply: @escaping (SPUUserUpdateChoice) -> Void) {
        guard !appcastItem.isInformationOnlyUpdate else { reply(.skip); receiveError(); return }
        let version = appcastItem.displayVersionString.isEmpty ? appcastItem.versionString : appcastItem.displayVersionString
        if state.stage == .installing {
            receiveReady(version: version, reply: reply)
        } else {
            self.version = version; phase = .downloading; progress = 0
            // Downloads and verifies only. showReady holds the installation reply.
            reply(.install)
        }
    }
    public func showUpdateReleaseNotes(with downloadData: SPUDownloadData) {}
    public func showUpdateReleaseNotesFailedToDownloadWithError(_ error: Error) {}
    public func showUpdateNotFoundWithError(_ error: Error, acknowledgement: @escaping () -> Void) {
        phase = .current; cancellation = nil; message = ""; acknowledgement()
    }
    public func showUpdaterError(_ error: Error, acknowledgement: @escaping () -> Void) { receiveError(); acknowledgement() }
    public func showDownloadInitiated(cancellation: @escaping () -> Void) {
        self.cancellation = cancellation; received = 0; expected = 0
    }
    public func showDownloadDidReceiveExpectedContentLength(_ expectedContentLength: UInt64) { expected = expectedContentLength }
    public func showDownloadDidReceiveData(ofLength length: UInt64) {
        let (sum, overflow) = received.addingReportingOverflow(length)
        received = overflow ? .max : sum
        progress = expected == 0 ? 0 : min(1, Double(received) / Double(expected))
    }
    public func showDownloadDidStartExtractingUpdate() { progress = 1 }
    public func showExtractionReceivedProgress(_ progress: Double) {}
    public func showReady(toInstallAndRelaunch reply: @escaping (SPUUserUpdateChoice) -> Void) { receiveReady(version: version, reply: reply) }
    public func showInstallingUpdate(withApplicationTerminated applicationTerminated: Bool, retryTerminatingApplication: @escaping () -> Void) {}
    public func showUpdateInstalledAndRelaunched(_ relaunched: Bool, acknowledgement: @escaping () -> Void) { acknowledgement() }
    public func dismissUpdateInstallation() { cancellation = nil; readyReply = nil }
    public func showUpdateInFocus() {}
}

struct UpdatesView: View {
    @ObservedObject var updates: AppUpdates
    @ObservedObject var model: AppModel
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Альберт \(updates.currentVersion)").font(.system(size: 20, weight: .medium))
            Text(status).font(.system(size: 13)).foregroundStyle(.secondary)
            if updates.phase == .downloading { ProgressView(value: updates.progress).controlSize(.small) }
            if !updates.message.isEmpty { Text(updates.message).font(.system(size: 12)).foregroundStyle(.secondary).textSelection(.enabled) }
            HStack(spacing: 16) {
                Button("Закрыть") { model.closeUpdates() }.keyboardShortcut(.cancelAction).disabled(updates.phase == .installing)
                Spacer()
                if updates.phase == .ready {
                    Button("Установить и перезапустить") { updates.install() }
                        .keyboardShortcut(.return, modifiers: .command).disabled(!updates.canInstall)
                        .help("Установить и перезапустить · ⌘Return").accessibilityIdentifier("installUpdate")
                } else {
                    Button("Проверить") { updates.check() }.keyboardShortcut("r", modifiers: .command)
                        .disabled(!updates.canCheck).help("Проверить обновления · ⌘R").accessibilityIdentifier("checkUpdates")
                }
            }.padding(.top, 8)
        }.padding(28).frame(width: 430).background(Palette.surface)
            .interactiveDismissDisabled(updates.phase == .installing)
    }
    private var status: String {
        switch updates.phase {
        case .unavailable: return "Обновления доступны в релизной сборке."
        case .idle: return "Обновления проверяются автоматически."
        case .checking: return "Проверка обновлений…"
        case .current: return "Установлена последняя версия."
        case .downloading: return "Загрузка версии \(updates.version)…"
        case .ready: return updates.canInstall ? "Версия \(updates.version) готова к установке." : "Дождитесь завершения текущей операции. Черновик будет сохранён."
        case .installing: return "Установка и перезапуск…"
        case .error: return "Обновление не завершено."
        }
    }
}
