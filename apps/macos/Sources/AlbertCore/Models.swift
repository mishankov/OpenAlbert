import Foundation

public struct ServerActivity: Codable, Sendable, Equatable {
    public let state: String
    public let text: String?
    public init(state: String, text: String? = nil) { self.state = state; self.text = text }
    public var isActive: Bool { state == "queued" || state == "processing" }
}

public struct ServerStatus: Codable, Sendable {
    public let apiVersion: Int
    public let name: String
    public let conversationID: String
    public let maxAttachmentBytes: Int
    public let activity: ServerActivity?
    enum CodingKeys: String, CodingKey {
        case activity, apiVersion = "api_version", name, conversationID = "conversation_id", maxAttachmentBytes = "max_attachment_bytes"
    }
}

public struct Attachment: Codable, Identifiable, Equatable, Sendable {
    public var id: String
    public var name: String
    public var mime: String
    public var sizeBytes: Int
    public var isImage: Bool { mime.lowercased().hasPrefix("image/") }
    public var isAudio: Bool { mime.lowercased().hasPrefix("audio/") }
    public init(id: String, name: String, mime: String, sizeBytes: Int) {
        self.id = id; self.name = name; self.mime = mime; self.sizeBytes = sizeBytes
    }
    enum CodingKeys: String, CodingKey { case id, name, mime, sizeBytes = "size_bytes" }
}

public struct Message: Codable, Identifiable, Equatable, Sendable {
    public var id: String
    public var role: String
    public var text: String
    public var format: String
    public var createdAt: Date
    public var delivery: String
    public var attachments: [Attachment]
    public var taskID: String?
    public init(id: String, role: String, text: String, format: String = "plain", createdAt: Date = Date(), delivery: String = "accepted", attachments: [Attachment] = [], taskID: String? = nil) {
        self.id = id; self.role = role; self.text = text; self.format = format; self.createdAt = createdAt
        self.delivery = delivery; self.attachments = attachments; self.taskID = taskID
    }
    enum CodingKeys: String, CodingKey { case id, role, text, format, createdAt = "created_at", delivery, attachments, taskID = "task_id" }
}

public struct MessagePage: Codable, Sendable {
    public var messages: [Message]
    public var hasMore: Bool
    public var nextBefore: String?
    public init(messages: [Message], hasMore: Bool = false, nextBefore: String? = nil) {
        self.messages = messages; self.hasMore = hasMore; self.nextBefore = nextBefore
    }
    enum CodingKeys: String, CodingKey { case messages, hasMore = "has_more", nextBefore = "next_before" }
}

public struct AlbertTask: Codable, Identifiable, Equatable, Sendable {
    public var id: String
    public var title: String
    public var goal: String
    public var status: String
    public var reason: String?
    public var result: String?
    public var scheduleSeconds: Int?
    public var nextRunAt: Date?
    public var createdAt: Date?
    public var updatedAt: Date?
    public var version: Int64
    public var isFinished: Bool { ["completed", "cancelled", "failed"].contains(status) }
    public var statusLabel: String {
        switch status {
        case "queued": return "В очереди"
        case "running": return "В работе"
        case "waiting": return "Ждёт ответа"
        case "paused": return "Приостановлено"
        case "completed": return "Готово"
        case "cancelled": return "Отменено"
        case "failed": return "Не выполнено"
        case "blocked": return "Нужна помощь"
        default: return status
        }
    }
    enum CodingKeys: String, CodingKey {
        case id, title, goal, status, reason, result, version
        case scheduleSeconds = "schedule_seconds", nextRunAt = "next_run_at", createdAt = "created_at", updatedAt = "updated_at"
    }
}

public struct MemoryEntry: Codable, Identifiable, Equatable, Sendable {
    public var path: String
    public var content: String
    public var updatedAt: Date?
    public var id: String { path }
    enum CodingKeys: String, CodingKey { case path, content, updatedAt = "updated_at" }
}

public struct SendRequest: Codable, Equatable, Sendable {
    public var clientMessageID: String
    public var text: String
    public var attachmentIDs: [String]
    public var taskID: String?
    public init(clientMessageID: String = UUID().uuidString.lowercased(), text: String, attachmentIDs: [String] = [], taskID: String? = nil) {
        self.clientMessageID = clientMessageID; self.text = text; self.attachmentIDs = attachmentIDs; self.taskID = taskID
    }
    enum CodingKeys: String, CodingKey { case clientMessageID = "client_message_id", text, attachmentIDs = "attachment_ids", taskID = "task_id" }
    public func matches(text: String, attachments: [Attachment], taskID: String?) -> Bool {
        self.text == text && attachmentIDs == attachments.map(\.id) && self.taskID == taskID
    }
}

public struct VoiceDraft: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var filename: String
    public var durationSeconds: Double
    public var uploadedAttachment: Attachment?
    public init(id: String = UUID().uuidString.lowercased(), filename: String? = nil, durationSeconds: Double, uploadedAttachment: Attachment? = nil) {
        self.id = id; self.filename = filename ?? "voice-\(id).m4a"
        self.durationSeconds = durationSeconds; self.uploadedAttachment = uploadedAttachment
    }
    public func fileURL(in store: LocalStore) -> URL? {
        guard UUID(uuidString: id) != nil, filename == "voice-\(id).m4a" else { return nil }
        return store.directory.appendingPathComponent("Voice", isDirectory: true).appendingPathComponent(filename)
    }
}

public struct Draft: Codable, Equatable, Sendable {
    public var text = ""
    public var attachments: [Attachment] = []
    public var taskID: String?
    public var pending: SendRequest?
    public var voice: VoiceDraft?
    public init() {}
    public var sendAttachments: [Attachment] { attachments + (voice?.uploadedAttachment.map { [$0] } ?? []) }
    public var hasContent: Bool { !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !attachments.isEmpty || voice != nil }
    public mutating func request() -> SendRequest {
        if let pending, pending.matches(text: text, attachments: sendAttachments, taskID: taskID) { return pending }
        let request = SendRequest(text: text, attachmentIDs: sendAttachments.map(\.id), taskID: taskID)
        pending = request
        return request
    }
}

public enum APIJSON {
    public static func decoder() -> JSONDecoder {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let value = try decoder.singleValueContainer().decode(String.self)
            let fractional = ISO8601DateFormatter()
            fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            if let date = fractional.date(from: value) ?? ISO8601DateFormatter().date(from: value) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "Invalid RFC 3339 date"))
        }
        return decoder
    }
    public static func encoder() -> JSONEncoder {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .custom { date, encoder in
            let formatter = ISO8601DateFormatter()
            formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            var container = encoder.singleValueContainer()
            try container.encode(formatter.string(from: date))
        }
        return encoder
    }
}
