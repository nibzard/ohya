import SwiftUI

enum OhyaTheme {
    static let brandPurple = Color(red: 0x82 / 255.0, green: 0x50 / 255.0, blue: 0xDF / 255.0)
    static let popoverWidth: CGFloat = 348
    static let popoverMaxHeight: CGFloat = 468
}

struct GlassControlSurface: ViewModifier {
    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency
    let cornerRadius: CGFloat

    func body(content: Content) -> some View {
        if reduceTransparency {
            content
                .background(
                    RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                        .fill(Color(nsColor: .controlBackgroundColor))
                )
                .overlay(
                    RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                        .strokeBorder(Color.primary.opacity(0.14), lineWidth: 0.75)
                )
        } else {
            adaptiveGlass(content)
        }
    }

    @ViewBuilder
    private func adaptiveGlass(_ content: Content) -> some View {
        #if compiler(>=6.2)
        if #available(macOS 26.0, *) {
            content
                .glassEffect(.regular, in: RoundedRectangle(cornerRadius: cornerRadius, style: .continuous))
        } else {
            fallbackSurface(content)
        }
        #else
        fallbackSurface(content)
        #endif
    }

    private func fallbackSurface(_ content: Content) -> some View {
        content
            .background(
                RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                    .fill(.ultraThinMaterial)
            )
            .overlay(
                RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                    .strokeBorder(Color.primary.opacity(0.08), lineWidth: 0.5)
            )
    }
}

struct GlassBarContainer<Content: View>: View {
    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency
    let content: () -> Content

    init(@ViewBuilder content: @escaping () -> Content) {
        self.content = content
    }

    var body: some View {
        if reduceTransparency {
            content()
        } else {
            adaptiveContainer
        }
    }

    @ViewBuilder
    private var adaptiveContainer: some View {
        #if compiler(>=6.2)
        if #available(macOS 26.0, *) {
            GlassEffectContainer(spacing: 6) {
                content()
            }
        } else {
            content()
        }
        #else
        content()
        #endif
    }
}

struct QuietRowHighlight: ViewModifier {
    let isHovered: Bool
    let isSelected: Bool
    let cornerRadius: CGFloat

    func body(content: Content) -> some View {
        content
            .background(
                RoundedRectangle(cornerRadius: cornerRadius, style: .continuous)
                    .fill(fillColor)
            )
    }

    private var fillColor: Color {
        if isSelected {
            return Color.accentColor.opacity(0.14)
        }
        if isHovered {
            return Color.primary.opacity(0.055)
        }
        return .clear
    }
}

struct CopyFeedbackButton: View {
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    let label: String
    let copiedLabel: String
    let systemImage: String
    let action: () -> Void

    @State private var copied = false

    init(
        _ label: String,
        copiedLabel: String = "Copied",
        systemImage: String = "doc.on.doc",
        action: @escaping () -> Void
    ) {
        self.label = label
        self.copiedLabel = copiedLabel
        self.systemImage = systemImage
        self.action = action
    }

    var body: some View {
        Button {
            action()
            triggerFeedback()
        } label: {
            Label(copied ? copiedLabel : label, systemImage: copied ? "checkmark" : systemImage)
                .font(.caption.weight(.medium))
                .contentTransition(reduceMotion ? .identity : .symbolEffect(.replace))
        }
        .buttonStyle(.borderless)
        .foregroundStyle(copied ? Color.green : Color.secondary)
    }

    private func triggerFeedback() {
        withAnimation(reduceMotion ? nil : .snappy(duration: 0.2)) {
            copied = true
        }
        Task {
            try? await Task.sleep(for: .seconds(1.5))
            await MainActor.run {
                withAnimation(reduceMotion ? nil : .easeOut(duration: 0.2)) {
                    copied = false
                }
            }
        }
    }
}

extension View {
    func ohyaGlassControl(cornerRadius: CGFloat = 10) -> some View {
        modifier(GlassControlSurface(cornerRadius: cornerRadius))
    }

    func ohyaRowHighlight(hovered: Bool, selected: Bool = false, cornerRadius: CGFloat = 8) -> some View {
        modifier(QuietRowHighlight(isHovered: hovered, isSelected: selected, cornerRadius: cornerRadius))
    }
}
