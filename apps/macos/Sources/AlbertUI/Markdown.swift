import SwiftUI

public enum MarkdownBlock: Equatable {
    case paragraph(String), heading(Int, String), code(String, String), quote(String), item(String, String), rule
}

/// A deliberately small, local renderer: raw HTML stays text and images never trigger remote requests.
public enum MarkdownParser {
    public static func blocks(_ input: String) -> [MarkdownBlock] {
        let lines = input.replacingOccurrences(of: "\r\n", with: "\n").components(separatedBy: "\n")
        var blocks: [MarkdownBlock] = [], paragraph: [String] = [], code: [String] = []
        var fence: String?, language = ""
        func flush() { if !paragraph.isEmpty { blocks.append(.paragraph(paragraph.joined(separator: "\n"))); paragraph = [] } }
        for line in lines {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if let currentFence = fence {
                if trimmed.hasPrefix(currentFence) { blocks.append(.code(language, code.joined(separator: "\n"))); fence = nil; code = [] }
                else { code.append(line) }
            } else if trimmed.hasPrefix("```") || trimmed.hasPrefix("~~~") {
                flush(); fence = String(trimmed.prefix(3)); language = String(trimmed.dropFirst(3)).trimmingCharacters(in: .whitespaces)
            } else if trimmed.isEmpty { flush() }
            else if ["---", "***", "___"].contains(trimmed) { flush(); blocks.append(.rule) }
            else if trimmed.hasPrefix("> ") || trimmed == ">" { flush(); blocks.append(.quote(String(trimmed.dropFirst(min(2, trimmed.count))))) }
            else if let heading = heading(trimmed) { flush(); blocks.append(heading) }
            else if trimmed.hasPrefix("- ") || trimmed.hasPrefix("* ") || trimmed.hasPrefix("+ ") { flush(); blocks.append(.item("•", String(trimmed.dropFirst(2)))) }
            else if let range = trimmed.range(of: #"^\d+[.)]\s+"#, options: .regularExpression) {
                flush(); blocks.append(.item(String(trimmed[range]).trimmingCharacters(in: .whitespaces), String(trimmed[range.upperBound...])))
            } else { paragraph.append(line) }
        }
        flush()
        if fence != nil { blocks.append(.code(language, code.joined(separator: "\n"))) }
        return blocks
    }
    private static func heading(_ line: String) -> MarkdownBlock? {
        let hashes = line.prefix(while: { $0 == "#" }).count
        guard (1...6).contains(hashes), line.dropFirst(hashes).first == " " else { return nil }
        return .heading(hashes, String(line.dropFirst(hashes + 1)))
    }
}

struct MarkdownView: View {
    var text: String
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            ForEach(Array(MarkdownParser.blocks(text).enumerated()), id: \.offset) { _, block in
                blockView(block)
            }
        }
        .textSelection(.enabled)
        .environment(\.openURL, OpenURLAction { url in
            // No custom schemes or file URLs from server text.
            guard ["https", "http", "mailto"].contains(url.scheme?.lowercased() ?? "") else { return .discarded }
            return .systemAction
        })
    }
    private func inline(_ source: String) -> Text {
        let options = AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace)
        if let parsed = try? AttributedString(markdown: source, options: options) { return Text(parsed) }
        return Text(source)
    }
    @ViewBuilder private func blockView(_ block: MarkdownBlock) -> some View {
        switch block {
        case .paragraph(let text): inline(text).lineSpacing(6).fixedSize(horizontal: false, vertical: true)
        case .heading(let level, let text):
            inline(text).font(.system(size: level <= 2 ? 22 : 16, weight: .medium)).padding(.top, level <= 2 ? 0 : 8)
        case .quote(let text):
            HStack(alignment: .top, spacing: 14) { Rectangle().fill(Color.secondary.opacity(0.25)).frame(width: 2); inline(text).foregroundStyle(.secondary).lineSpacing(5) }.fixedSize(horizontal: false, vertical: true)
        case .item(let marker, let text): HStack(alignment: .firstTextBaseline, spacing: 10) { Text(marker).frame(minWidth: 14, alignment: .trailing); inline(text).lineSpacing(5) }
        case .code(_, let code):
            ScrollView(.horizontal) { Text(code).font(.system(size: 13, design: .monospaced)).lineSpacing(5).padding(16).frame(maxWidth: .infinity, alignment: .leading) }
                .background(Palette.quiet, in: RoundedRectangle(cornerRadius: 8))
        case .rule: Divider().padding(.vertical, 5)
        }
    }
}
