import AppKit
import SwiftUI

@MainActor
public enum KeyboardGate {
    public static func permitsPrimaryAction() -> Bool {
        permitsPrimaryAction(event: NSApp?.currentEvent, responder: NSApp?.keyWindow?.firstResponder)
    }
    public static func permitsPrimaryAction(event: NSEvent?, responder: NSResponder?) -> Bool {
        if event?.type == .keyDown && event?.isARepeat == true { return false }
        if let input = responder as? NSTextInputClient, input.hasMarkedText() { return false }
        return true
    }
}

/// NSTextView keeps system text editing, input methods and non-English keyboard layouts.
public final class ComposerTextView: NSTextView {
    public var onSend: (() -> Void)?
    public var onEscape: (() -> Void)?
    public var onHeightChange: ((CGFloat) -> Void)?
    public var onFocus: (() -> Void)?
    public override func becomeFirstResponder() -> Bool { let accepted = super.becomeFirstResponder(); if accepted { onFocus?() }; return accepted }
    public override func setFrameSize(_ newSize: NSSize) { super.setFrameSize(newSize); reportHeight() }
    public func reportHeight() {
        guard let container = textContainer, let manager = layoutManager, container.containerSize.width > 0 else { return }
        manager.ensureLayout(for: container)
        let height = min(120, max(32, ceil(manager.usedRect(for: container).height + textContainerInset.height * 2)))
        onHeightChange?(height)
    }
    public override func performKeyEquivalent(with event: NSEvent) -> Bool {
        let modifiers = event.modifierFlags.intersection([.command, .control, .option, .shift])
        if event.type == .keyDown, modifiers == .command, [36, 76].contains(event.keyCode) {
            if KeyboardGate.permitsPrimaryAction(event: event, responder: self) { onSend?() }
            return true
        }
        return super.performKeyEquivalent(with: event)
    }
    public override func keyDown(with event: NSEvent) {
        let modifiers = event.modifierFlags.intersection([.command, .control, .option, .shift])
        if [36, 76].contains(event.keyCode), modifiers == .shift, !hasMarkedText() {
            insertNewline(nil)
            return
        }
        if [36, 76].contains(event.keyCode), modifiers.isEmpty, !hasMarkedText() {
            if KeyboardGate.permitsPrimaryAction(event: event, responder: self) { onSend?() }
            return
        }
        if event.keyCode == 53, !hasMarkedText() { onEscape?(); return }
        super.keyDown(with: event)
    }
}

struct ComposerEditor: NSViewRepresentable {
    @Binding var text: String
    @ObservedObject var model: AppModel
    func makeCoordinator() -> Coordinator { Coordinator(self) }
    func makeNSView(context: Context) -> NSScrollView {
        let scroll = NSScrollView()
        let editor = ComposerTextView()
        editor.isRichText = false; editor.drawsBackground = false
        editor.font = .systemFont(ofSize: 14)
        editor.textColor = .labelColor; editor.insertionPointColor = .labelColor
        editor.textContainerInset = NSSize(width: 0, height: 7)
        editor.textContainer?.lineFragmentPadding = 0
        editor.isVerticallyResizable = true; editor.isHorizontallyResizable = false
        editor.autoresizingMask = [.width]
        editor.textContainer?.widthTracksTextView = true
        editor.minSize = NSSize(width: 0, height: 32)
        editor.maxSize = NSSize(width: CGFloat.greatestFiniteMagnitude, height: CGFloat.greatestFiniteMagnitude)
        editor.delegate = context.coordinator
        editor.string = text
        editor.setAccessibilityLabel("Сообщение Альберту")
        editor.setAccessibilityIdentifier("messageEditor")
        editor.setAccessibilityHelp("Отправить: Enter. Новая строка: Shift Enter.")
        editor.onSend = { model.submit() }
        editor.onEscape = { model.escape() }
        editor.onFocus = { model.selectedDraftAttachmentID = nil }
        editor.onHeightChange = { height in
            guard abs(model.composerHeight - height) > 0.5 else { return }
            DispatchQueue.main.async { if abs(model.composerHeight - height) > 0.5 { model.composerHeight = height } }
        }
        scroll.documentView = editor
        scroll.drawsBackground = false; scroll.hasVerticalScroller = true
        scroll.scrollerStyle = .overlay; scroll.borderType = .noBorder
        model.composer = editor
        return scroll
    }
    func updateNSView(_ scroll: NSScrollView, context: Context) {
        context.coordinator.parent = self
        guard let editor = scroll.documentView as? ComposerTextView else { return }
        editor.isEditable = model.updates.phase != .installing
        if editor.string != text && !editor.hasMarkedText() {
            let selection = editor.selectedRange()
            editor.string = text
            editor.setSelectedRange(NSRange(location: min(selection.location, (text as NSString).length), length: 0))
        }
        editor.reportHeight()
        if context.coordinator.focusVersion != model.focusComposer {
            context.coordinator.focusVersion = model.focusComposer
            DispatchQueue.main.async { editor.window?.makeFirstResponder(editor) }
        }
    }
    final class Coordinator: NSObject, NSTextViewDelegate {
        var parent: ComposerEditor
        var focusVersion = -1
        init(_ parent: ComposerEditor) { self.parent = parent }
        func textDidChange(_ notification: Notification) {
            guard let editor = notification.object as? NSTextView else { return }
            parent.model.selectedDraftAttachmentID = nil
            parent.text = editor.string
            (editor as? ComposerTextView)?.reportHeight()
        }
    }
}
