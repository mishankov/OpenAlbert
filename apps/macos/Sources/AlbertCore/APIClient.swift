import Foundation

public enum APIError: LocalizedError, Equatable {
    case invalidURL, unauthorized, invalidResponse, tooLarge
    case server(Int, String)
    public var errorDescription: String? {
        switch self {
        case .invalidURL: return "Введите HTTP- или HTTPS-адрес сервера."
        case .unauthorized: return "Проверьте токен в настройках подключения."
        case .invalidResponse: return "Сервер вернул неожиданный ответ."
        case .tooLarge: return "Размер файла превышает 20 МБ."
        case .server(_, let message): return message
        }
    }
}

/// Bearer credentials never follow redirects, including redirects to a different origin.
public final class NoRedirectDelegate: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
    public func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
        completionHandler(nil)
    }
}

public final class APIClient: @unchecked Sendable {
    public let baseURL: URL
    private let token: String
    private let session: URLSession
    public static let attachmentLimit = 20 * 1024 * 1024
    private static let delegate = NoRedirectDelegate()

    public static func validatedURL(_ value: String) throws -> URL {
        guard let parts = URLComponents(string: value.trimmingCharacters(in: .whitespacesAndNewlines)),
              let scheme = parts.scheme?.lowercased(), let host = parts.host, !host.isEmpty,
              parts.user == nil, parts.password == nil, parts.query == nil, parts.fragment == nil,
              parts.path.isEmpty || parts.path == "/",
              scheme == "https" || scheme == "http",
              let url = parts.url else { throw APIError.invalidURL }
        return url
    }

    public init(baseURL: URL, token: String, session: URLSession? = nil) throws {
        self.baseURL = try Self.validatedURL(baseURL.absoluteString)
        self.token = token
        if let session { self.session = session }
        else {
            let configuration = URLSessionConfiguration.ephemeral
            configuration.timeoutIntervalForRequest = 30
            configuration.timeoutIntervalForResource = 60
            configuration.httpCookieStorage = nil
            configuration.urlCache = nil
            self.session = URLSession(configuration: configuration, delegate: Self.delegate, delegateQueue: nil)
        }
    }

    private func endpoint(_ components: [String], query: [URLQueryItem] = []) -> URL {
        var parts = URLComponents(url: baseURL, resolvingAgainstBaseURL: false)!
        let allowed = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "-_~"))
        parts.percentEncodedPath = (["api", "v1"] + components).map { $0.addingPercentEncoding(withAllowedCharacters: allowed)! }.joined(separator: "/").withLeadingSlash
        parts.queryItems = query.isEmpty ? nil : query
        return parts.url!
    }

    private func fetch(_ components: [String], method: String = "GET", body: Data? = nil, contentType: String? = nil, query: [URLQueryItem] = []) async throws -> (Data, HTTPURLResponse) {
        var request = URLRequest(url: endpoint(components, query: query))
        request.httpMethod = method
        request.httpBody = body
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        if let contentType { request.setValue(contentType, forHTTPHeaderField: "Content-Type") }
        let (data, response) = try await session.data(for: request)
        guard let http = response as? HTTPURLResponse else { throw APIError.invalidResponse }
        guard (200..<300).contains(http.statusCode) else {
            if http.statusCode == 401 || http.statusCode == 403 { throw APIError.unauthorized }
            struct ErrorEnvelope: Decodable { struct Details: Decodable { let message: String }; let error: Details }
            let error = try? JSONDecoder().decode(ErrorEnvelope.self, from: data)
            throw APIError.server(http.statusCode, error?.error.message ?? "Сервер недоступен (\(http.statusCode)).")
        }
        return (data, http)
    }
    private func get<T: Decodable>(_ path: String, query: [URLQueryItem] = []) async throws -> T {
        let (data, _) = try await fetch([path], query: query)
        return try APIJSON.decoder().decode(T.self, from: data)
    }
    public func status() async throws -> ServerStatus { try await get("status") }
    public func messages(before: String? = nil) async throws -> MessagePage {
        var query = [URLQueryItem(name: "limit", value: "100")]
        if let before { query.append(URLQueryItem(name: "before", value: before)) }
        return try await get("messages", query: query)
    }
    public func tasks() async throws -> [AlbertTask] {
        struct Response: Decodable { let tasks: [AlbertTask] }
        let value: Response = try await get("tasks"); return value.tasks
    }
    public func memory() async throws -> [MemoryEntry] {
        struct Response: Decodable { let entries: [MemoryEntry] }
        let value: Response = try await get("memory"); return value.entries
    }
    public func files() async throws -> [Attachment] {
        struct Response: Decodable { let files: [Attachment] }
        let value: Response = try await get("files"); return value.files
    }
    public func send(_ request: SendRequest) async throws -> Message {
        struct Response: Decodable { let message: Message }
        let (data, _) = try await fetch(["messages"], method: "POST", body: APIJSON.encoder().encode(request), contentType: "application/json")
        return try APIJSON.decoder().decode(Response.self, from: data).message
    }
    public func stop(id: String) async throws {
        struct Request: Encodable { let client_message_id: String }
        _ = try await fetch(["stop"], method: "POST", body: JSONEncoder().encode(Request(client_message_id: id)), contentType: "application/json")
    }
    public func upload(data: Data, name: String, mime: String) async throws -> Attachment {
        guard data.count <= Self.attachmentLimit else { throw APIError.tooLarge }
        let boundary = "albert-\(UUID().uuidString)"
        let safeName = name.replacingOccurrences(of: "\"", with: "_").replacingOccurrences(of: "\r", with: "_").replacingOccurrences(of: "\n", with: "_")
        let safeMIME = mime.contains("\r") || mime.contains("\n") ? "application/octet-stream" : mime
        var body = Data("--\(boundary)\r\nContent-Disposition: form-data; name=\"file\"; filename=\"\(safeName)\"\r\nContent-Type: \(safeMIME)\r\n\r\n".utf8)
        body.append(data); body.append(Data("\r\n--\(boundary)--\r\n".utf8))
        struct Response: Decodable { let attachment: Attachment }
        let (result, _) = try await fetch(["attachments"], method: "POST", body: body, contentType: "multipart/form-data; boundary=\(boundary)")
        return try APIJSON.decoder().decode(Response.self, from: result).attachment
    }
    public func download(_ attachment: Attachment) async throws -> Data {
        let (data, _) = try await fetch(["attachments", attachment.id, "content"])
        guard data.count <= Self.attachmentLimit else { throw APIError.tooLarge }
        return data
    }
}

private extension String {
    var withLeadingSlash: String { "/" + self }
}

public struct CachedContent: Codable {
    public var nextBefore: String?
    public var messages: [Message] = []
    public var tasks: [AlbertTask] = []
    public var memory: [MemoryEntry] = []
    public var files: [Attachment] = []
    public init() {}
    public mutating func merge(_ page: MessagePage) {
        var byID = Dictionary(messages.map { ($0.id, $0) }, uniquingKeysWith: { _, last in last })
        for message in page.messages { byID[message.id] = message }
        messages = byID.values.sorted { $0.createdAt == $1.createdAt ? $0.id < $1.id : $0.createdAt < $1.createdAt }
    }
}

public struct LocalStore {
    public let directory: URL
    public init(directory: URL) { self.directory = directory }
    public func load<T: Decodable>(_ type: T.Type, name: String) -> T? {
        guard let data = try? Data(contentsOf: directory.appendingPathComponent(name)) else { return nil }
        return try? APIJSON.decoder().decode(type, from: data)
    }
    public func save<T: Encodable>(_ value: T, name: String) throws {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let url = directory.appendingPathComponent(name)
        try APIJSON.encoder().encode(value).write(to: url, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }
}
