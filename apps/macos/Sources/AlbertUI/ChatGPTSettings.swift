import AppKit
import SwiftUI
import AlbertCore

@MainActor
public final class ChatGPTSettings: ObservableObject {
    public var controlsVisible = false
    @Published public var status: ChatGPTStatus?
    @Published public var models: [ChatGPTModel] = []
    @Published public var callback = "" // Memory only; survives closing settings, never saved to the chat or disk.
    @Published public var pendingURL: URL?
    @Published public var busy = false
    @Published public var error: String?
    public var openBrowser: (URL) -> Bool = { NSWorkspace.shared.open($0) }
    private var server: URL?
    private var pendingExpiry = Date.distantPast

    public func load(_ client: APIClient) async {
        guard !busy else { return }
        if server != client.baseURL { status = nil; models = []; callback = ""; pendingURL = nil; server = client.baseURL }
        busy = true; defer { busy = false }
        do {
            status = try await client.chatGPTStatus(); error = nil
            if status?.siwc.planEnabled == true { models = try await client.chatGPTModels() }
        } catch { self.error = error.localizedDescription }
    }
    public func begin(_ client: APIClient) {
        guard controlsVisible, !busy, let mode = status?.mode, mode != "api_key", KeyboardGate.permitsPrimaryAction() else { return }
        busy = true; error = nil
        Task {
            defer { busy = false }
            do {
                let url: URL
                if let pendingURL, pendingExpiry > Date() { url = pendingURL } else { url = try await client.chatGPTBegin(); pendingURL = url; pendingExpiry = Date().addingTimeInterval(600) }
                if !openBrowser(url) { error = "Не удалось открыть браузер. Повторите вход." }
            } catch { self.error = error.localizedDescription }
        }
    }
    public func finish(_ client: APIClient) {
        guard controlsVisible, !busy, !callback.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty, KeyboardGate.permitsPrimaryAction() else { return }
        busy = true; error = nil
        Task {
            defer { busy = false }
            do {
                status = try await client.chatGPTFinish(callback)
                callback = ""; pendingURL = nil
                if status?.siwc.planEnabled == true { models = try await client.chatGPTModels() }
            } catch { self.error = error.localizedDescription; pendingURL = nil }
        }
    }
    public func select(_ slug: String, client: APIClient) {
        guard !busy, slug != status?.siwc.model, KeyboardGate.permitsPrimaryAction() else { return }
        busy = true; error = nil
        Task {
            defer { busy = false }
            do { status = try await client.chatGPTModel(slug) }
            catch { self.error = error.localizedDescription }
        }
    }
}

struct ChatGPTSettingsView: View {
    @ObservedObject var settings: ChatGPTSettings
    let client: APIClient
    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Оплата инференса").font(.headline)
            if let status = settings.status {
                Text("Активно: \(status.activeLane == "siwc" ? "план ChatGPT" : "API-ключ")").font(.subheadline)
                if status.mode == "api_key" {
                    Text("Для входа через ChatGPT включите siwc или auto на сервере.").font(.caption).foregroundStyle(.secondary)
                } else {
                    Text(status.siwc.planEnabled ? "План ChatGPT подключён" : status.siwc.connected ? "Вход сохранён, оплата планом не разрешена. Войдите снова или используйте API-ключ." : "Войдите, чтобы использовать лимит плана ChatGPT.").font(.caption)
                    Text(status.mode == "auto" ? "При недоступности плана запрос оплачивается API-ключом." : "Запросы расходуют общий лимит ChatGPT и Codex.").font(.caption).foregroundStyle(.secondary)
                    Button("Continue with ChatGPT") { settings.begin(client) }
                        .keyboardShortcut("l", modifiers: [.command, .shift])
                        .help("Continue with ChatGPT · ⌘⇧L").accessibilityIdentifier("chatGPTSignIn")
                        .disabled(settings.busy)
                    Text("После согласия страница 127.0.0.1 может не открыться. Вставьте её полный адрес в течение 10 минут.").font(.caption)
                    TextField("Адрес из браузера", text: $settings.callback).textFieldStyle(.roundedBorder).accessibilityIdentifier("chatGPTCallback")
                    Button("Завершить вход") { settings.finish(client) }
                        .keyboardShortcut(.return, modifiers: [.command, .shift])
                        .help("Завершить вход · ⌘⇧Return").accessibilityIdentifier("chatGPTFinish")
                        .disabled(settings.busy || settings.callback.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                    if !settings.models.isEmpty {
                        Picker("Модель ChatGPT", selection: Binding(get: { status.siwc.model }, set: { settings.select($0, client: client) })) {
                            if !settings.models.contains(where: { $0.slug == status.siwc.model }) { Text(status.siwc.model).tag(status.siwc.model) }
                            ForEach(settings.models) { Text($0.displayName).tag($0.slug) }
                        }.disabled(settings.busy).accessibilityIdentifier("chatGPTModel")
                    }
                }
                Text("С запуска: ChatGPT \(status.siwcRequests), API-ключ \(status.apiKeyRequests)").font(.caption).foregroundStyle(.secondary)
                if let last = status.last429 { Text("Последний 429: \(last.formatted())").font(.caption) }
                if let retry = status.retryAt { Text("Повтор через план после \(retry.formatted()). Это локальная пауза, не время сброса лимита.").font(.caption) }
                if status.planIneligible == true {
                    Text("План недоступен этому аккаунту или рабочему пространству. Автоматических повторов не будет; проверьте доступность перед повторным входом.").font(.caption)
                }
                if let code = status.lastErrorCode { Text("Последняя ошибка: \(code)").font(.caption).textSelection(.enabled) }
                if let id = status.lastRequestID { Text("ID запроса: \(id)").font(.caption).textSelection(.enabled) }
                Link("Лимиты ChatGPT", destination: URL(string: "https://chatgpt.com/settings/usage")!).font(.caption)
            }
            if settings.busy { ProgressView().controlSize(.small) }
            if let error = settings.error { Text(error).font(.caption).foregroundStyle(.secondary).textSelection(.enabled) }
        }.task { settings.controlsVisible = true; await settings.load(client) }
        .onDisappear { settings.controlsVisible = false }
    }
}
