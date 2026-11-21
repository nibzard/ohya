import AppKit
import Foundation

@MainActor
final class ClipboardGuard {
    private static let transientType = NSPasteboard.PasteboardType("org.nspasteboard.TransientType")
    private static let concealedType = NSPasteboard.PasteboardType("org.nspasteboard.ConcealedType")

    private var clearTask: Task<Void, Never>?

    func copyText(_ text: String) {
        clearTask?.cancel()
        let pasteboard = NSPasteboard.general
        pasteboard.clearContents()
        pasteboard.setString(text, forType: .string)
    }

    func copySecret(_ secret: String, autoClearSeconds: TimeInterval = 60) {
        clearTask?.cancel()
        let pasteboard = NSPasteboard.general
        pasteboard.clearContents()
        pasteboard.setString(secret, forType: .string)
        pasteboard.setData(Data(), forType: Self.transientType)
        pasteboard.setData(Data(), forType: Self.concealedType)
        let capturedChangeCount = pasteboard.changeCount

        clearTask = Task { [weak self] in
            try? await Task.sleep(for: .seconds(autoClearSeconds))
            guard !Task.isCancelled else { return }
            self?.clearIfUnchanged(expectedChangeCount: capturedChangeCount)
        }
    }

    private func clearIfUnchanged(expectedChangeCount: Int) {
        let pasteboard = NSPasteboard.general
        guard pasteboard.changeCount == expectedChangeCount else { return }
        pasteboard.clearContents()
    }
}
