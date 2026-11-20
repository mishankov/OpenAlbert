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

    public var usesProfiles: Bool { status?.mode == "profiles" }
    public var allowsSignIn: Bool {
        guard let status else { return false }
        if usesProfiles { return status.profiles?.values.contains { $0.authMode == "siwc" } == true }
        return status.mode != "api_key"
    }
    public var canBegin: Bool { controlsVisible && !busy && allowsSignIn }
    public var canFinish: Bool { canBegin && !callback.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }
    public var showsModelPicker: Bool { !usesProfiles && allowsSignIn && !models.isEmpty }
    var profileRows: [ChatGPTProfileRow] {
        let order = ["chat", "background", "analysis"]
        return (status?.profiles ?? [:]).map { ChatGPTProfileRow(id: $0.key, route: $0.value) }.sorted {
            let lhs = order.firstIndex(of: $0.id) ?? order.count
            let rhs = order.firstIndex(of: $1.id) ?? order.count
            return lhs == rhs ? $0.id < $1.id : lhs < rhs
        }
    }
    private func loadModels(_ client: APIClient) async throws {
        models = []
        if !usesProfiles && allowsSignIn && status?.siwc.planEnabled == true { models = try await client.chatGPTModels() }
    }

    public func load(_ client: APIClient) async {
        guard !busy else { return }
        if server != client.baseURL { status = nil; models = []; callback = ""; pendingURL = nil; server = client.baseURL }
        busy = true; defer { busy = false }
        do {
            status = try await client.chatGPTStatus(); error = nil
            try await loadModels(client)
        } catch { self.error = error.localizedDescription }
    }
    public func begin(_ client: APIClient) {
        guard canBegin, KeyboardGate.permitsPrimaryAction() else { return }
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
        guard canFinish, KeyboardGate.permitsPrimaryAction() else { return }
        busy = true; error = nil
        Task {
            defer { busy = false }
            do {
                status = try await client.chatGPTFinish(callback)
                callback = ""; pendingURL = nil
                try await loadModels(client)
            } catch { self.error = error.localizedDescription; pendingURL = nil }
        }
    }
    public func select(_ slug: String, client: APIClient) {
        guard !usesProfiles, !busy, slug != status?.siwc.model, KeyboardGate.permitsPrimaryAction() else { return }
        busy = true; error = nil
        Task {
            defer { busy = false }
            do { status = try await client.chatGPTModel(slug) }
            catch { self.error = error.localizedDescription }
        }
    }
}

struct ChatGPTProfileRow: Identifiable {
    let id: String
    let route: ChatGPTStatus.Profile
    var title: String {
        switch id {
        case "chat": return "Разговор"
        case "background": return "Фоновые задачи"
        case "analysis": return "Анализ"
        default: return id
        }
    }
    var billing: String {
        switch route.authMode {
        case "siwc": return "План ChatGPT"
        case "api_key": return "API-ключ"
        default: return route.authMode
        }
    }
    var details: String { "Профиль: \(route.profile) · Модель: \(route.model) · Оплата: \(billing)" }
}

struct ChatGPTSettingsView: View {
    @ObservedObject var settings: ChatGPTSettings
    let client: APIClient
    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Оплата инференса").font(.headline)
            if let status = settings.status {
                if settings.usesProfiles {
                    Text("Оплата и модели по ролям").font(.subheadline)
                    ForEach(settings.profileRows) { row in
                        VStack(alignment: .leading, spacing: 3) {
                            Text(row.title).font(.subheadline)
                            Text(row.details).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                        }.accessibilityElement(children: .combine).accessibilityIdentifier("chatGPTProfile-\(row.id)")
                    }
                    if settings.profileRows.isEmpty {
                        Text("Сервер не передал настройки ролей.").font(.caption)
                    }
                    Text("Модели и способ оплаты каждой роли настраиваются на сервере в models.toml.").font(.caption).foregroundStyle(.secondary)
                    if !settings.allowsSignIn {
                        Text(settings.profileRows.isEmpty
                            ? "Чтобы войти в ChatGPT, настройте профиль с подпиской в models.toml и проверьте настройки ролей на сервере."
                            : "Вход в ChatGPT не требуется для настроенных ролей. Чтобы подключить план ChatGPT, выберите профиль с подпиской в models.toml на сервере.").font(.caption)
                    }
                } else {
                    Text("Активно: \(status.activeLane == "siwc" ? "план ChatGPT" : "API-ключ")").font(.subheadline)
                }
                if status.mode == "api_key" {
                    Text("Для входа через ChatGPT включите siwc или auto на сервере.").font(.caption).foregroundStyle(.secondary)
                } else if settings.allowsSignIn {
                    if settings.usesProfiles {
                        Text("Один вход в ChatGPT используется всеми ролями с оплатой планом.").font(.caption)
                    }
                    Text(status.siwc.planEnabled ? "План ChatGPT подключён" : status.siwc.connected ? "Вход сохранён, оплата планом не разрешена. Войдите снова или используйте API-ключ." : "Войдите, чтобы использовать лимит плана ChatGPT.").font(.caption)
                    Text(status.mode == "auto" ? "При недоступности плана запрос оплачивается API-ключом." : "Запросы расходуют общий лимит ChatGPT и Codex.").font(.caption).foregroundStyle(.secondary)
                    Button("Continue with ChatGPT") { settings.begin(client) }
                        .keyboardShortcut("l", modifiers: [.command, .shift])
                        .help("Continue with ChatGPT · ⌘⇧L").accessibilityIdentifier("chatGPTSignIn")
                        .disabled(!settings.canBegin)
                    Text("После согласия страница 127.0.0.1 может не открыться. Вставьте её полный адрес в течение 10 минут.").font(.caption)
                    TextField("Адрес из браузера", text: $settings.callback).textFieldStyle(.roundedBorder).accessibilityIdentifier("chatGPTCallback").disabled(settings.busy)
                    Button("Завершить вход") { settings.finish(client) }
                        .keyboardShortcut(.return, modifiers: [.command, .shift])
                        .help("Завершить вход · ⌘⇧Return").accessibilityIdentifier("chatGPTFinish")
                        .disabled(!settings.canFinish)
                    if settings.showsModelPicker {
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
