import AppKit
import OhyaMenuBarCore
import SwiftUI

struct PopoverRootView: View {
    @Bindable var model: AppModel
    @Environment(\.openWindow) private var openWindow
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        VStack(spacing: 0) {
            header
                .padding(.horizontal, 14)
                .padding(.top, 12)
                .padding(.bottom, 8)

            segmentedSwitcher
                .padding(.horizontal, 14)
                .padding(.bottom, 10)

            Divider()
                .opacity(0.45)

            contentArea
                .frame(maxHeight: OhyaTheme.popoverMaxHeight - 132)

            if let message = model.transientMessage {
                HStack(spacing: 6) {
                    Image(systemName: "checkmark.circle.fill")
                        .foregroundStyle(.green)
                    Text(message)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Spacer()
                }
                .padding(.horizontal, 14)
                .padding(.vertical, 6)
                .transition(.opacity)
            }

            Divider()
                .opacity(0.45)

            footer
                .padding(.horizontal, 10)
                .padding(.vertical, 8)
        }
        .animation(reduceMotion ? nil : .snappy(duration: 0.2), value: model.selectedSection)
        .animation(reduceMotion ? nil : .easeInOut(duration: 0.2), value: model.transientMessage)
        .sheet(isPresented: $model.showingLogs) {
            LogsSheetView(model: model)
        }
        .sheet(isPresented: $model.showingPurgeReview) {
            PurgeReviewSheet(model: model)
        }
        .background {
            keyboardShortcuts
        }
    }

    private var header: some View {
        HStack(spacing: 8) {
            Image(nsImage: MenuBarIconRenderer.render(state: .caughtUp))
                .resizable()
                .frame(width: 16, height: 16)
                .accessibilityHidden(true)

            Text("ohya")
                .font(.system(.headline, design: .rounded).weight(.semibold))

            Text(model.summary.topologyLabel)
                .font(.caption2.weight(.medium))
                .foregroundStyle(.secondary)
                .padding(.horizontal, 6)
                .padding(.vertical, 2)
                .background(Capsule().fill(Color.primary.opacity(0.07)))

            Spacer()

            HStack(spacing: 5) {
                Circle()
                    .fill(statusDotColor)
                    .frame(width: 7, height: 7)
                Text(model.summary.iconState.shortStatusLabel)
                    .font(.caption.weight(.medium))
                    .foregroundStyle(statusTextColor)
            }
            .accessibilityElement(children: .combine)
        }
    }

    private var segmentedSwitcher: some View {
        GlassBarContainer {
            HStack(spacing: 4) {
                ForEach(PopoverSection.allCases) { section in
                    let selected = model.selectedSection == section
                    Button {
                        model.selectSection(section)
                    } label: {
                        Text(sectionTitle(section))
                            .font(.subheadline.weight(selected ? .semibold : .regular))
                            .frame(maxWidth: .infinity)
                            .padding(.vertical, 5)
                            .foregroundStyle(selected ? .primary : .secondary)
                            .background {
                                if selected {
                                    RoundedRectangle(cornerRadius: 7, style: .continuous)
                                        .fill(Color.primary.opacity(0.1))
                                }
                            }
                    }
                    .buttonStyle(.plain)
                }
            }
            .padding(3)
            .ohyaGlassControl(cornerRadius: 10)
        }
    }

    @ViewBuilder
    private var contentArea: some View {
        switch model.selectedSection {
        case .status:
            StatusView(model: model)
        case .recall:
            RecallView(model: model)
        case .devices:
            DevicesView(model: model)
        }
    }

    private var footer: some View {
        GlassBarContainer {
            HStack(spacing: 4) {
                Button {
                    Task { await model.toggleService() }
                } label: {
                    Label(
                        model.summary.isPaused ? "Resume" : "Pause",
                        systemImage: model.summary.isPaused ? "play.fill" : "pause.fill"
                    )
                    .font(.caption.weight(.medium))
                }
                .buttonStyle(.borderless)
                .disabled(model.isTogglingService || model.resolvedBinaryPath == nil)
                .padding(.horizontal, 6)
                .padding(.vertical, 4)

                Spacer()

                Button {
                    Task { await model.openLogs() }
                } label: {
                    Image(systemName: "text.alignleft")
                        .font(.caption)
                }
                .buttonStyle(.borderless)
                .help("Recent warnings and errors")
                .padding(5)

                Button {
                    Task { await model.exportDiagnostics() }
                } label: {
                    Image(systemName: "stethoscope")
                        .font(.caption)
                }
                .buttonStyle(.borderless)
                .disabled(model.isExportingDiagnostics || model.resolvedBinaryPath == nil)
                .help("Export sanitized diagnostics bundle")
                .padding(5)

                Button {
                    openSettingsWindow()
                } label: {
                    Image(systemName: "gearshape")
                        .font(.caption)
                }
                .buttonStyle(.borderless)
                .help("Settings (⌘,)")
                .padding(5)

                Button {
                    NSApp.terminate(nil)
                } label: {
                    Image(systemName: "power")
                        .font(.caption)
                }
                .buttonStyle(.borderless)
                .help("Quit menu bar app (background capture keeps running)")
                .padding(5)
            }
            .foregroundStyle(.secondary)
        }
    }

    private var keyboardShortcuts: some View {
        Group {
            Button("") { model.selectSection(.status) }.keyboardShortcut("1", modifiers: .command)
            Button("") { model.selectSection(.recall) }.keyboardShortcut("2", modifiers: .command)
            Button("") { model.selectSection(.devices) }.keyboardShortcut("3", modifiers: .command)
            Button("") { model.selectSection(.recall) }.keyboardShortcut("k", modifiers: .command)
            Button("") { openSettingsWindow() }.keyboardShortcut(",", modifiers: .command)
            Button("") { Task { await model.refreshAll() } }.keyboardShortcut("r", modifiers: .command)
        }
        .opacity(0)
        .frame(width: 0, height: 0)
    }

    private func openSettingsWindow() {
        openWindow(id: "settings")
        NSApp.activate(ignoringOtherApps: true)
    }

    private func sectionTitle(_ section: PopoverSection) -> String {
        if section == .devices, let count = model.devicesData?.devices.filter({ $0.state == "paired" }).count, count > 0 {
            return "Devices (\(count))"
        }
        return section.title
    }

    private var statusDotColor: Color {
        switch model.summary.iconState {
        case .caughtUp: return .green
        case .syncing: return OhyaTheme.brandPurple
        case .degraded: return .orange
        case .paused: return .secondary.opacity(0.6)
        case .unavailable: return .red
        }
    }

    private var statusTextColor: Color {
        switch model.summary.iconState {
        case .degraded, .unavailable: return .primary
        default: return .secondary
        }
    }
}
