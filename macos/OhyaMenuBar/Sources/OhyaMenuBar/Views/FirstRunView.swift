import OhyaMenuBarCore
import SwiftUI

struct FirstRunView: View {
    let model: AppModel
    @State private var joining = false
    @State private var joinCode = ""

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Set up ohya")
                        .font(.headline)
                    Text("ohya keeps a shared memory of your coding-agent sessions. Set it up on this Mac, or join a hub that another machine runs.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }

                if let report = model.setupReport {
                    results(report)
                }

                if let error = model.setupError {
                    Label(error, systemImage: "exclamationmark.triangle.fill")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                }

                if model.isRunningSetup {
                    HStack(spacing: 8) {
                        ProgressView()
                            .controlSize(.small)
                        Text("Setting up ohya…")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                } else if joining {
                    joinForm
                } else if model.setupReport == nil {
                    choices
                }
            }
            .padding(14)
        }
    }

    private var choices: some View {
        VStack(spacing: 8) {
            choiceButton(
                title: "Use this Mac",
                detail: "Capture this Mac's sessions into memory on this Mac.",
                systemImage: "laptopcomputer"
            ) {
                Task { await model.runSetup(joinCode: nil) }
            }
            choiceButton(
                title: "Join a hub",
                detail: "Upload this Mac's sessions to a hub with a one-time join code.",
                systemImage: "network"
            ) {
                joining = true
            }
        }
    }

    private var joinForm: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("On the hub, run ohya devices invite, then paste the code here.")
                .font(.caption)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            SecureField("Join code", text: $joinCode)
                .textFieldStyle(.roundedBorder)
                .onSubmit(join)
            HStack {
                Button("Back") {
                    joinCode = ""
                    joining = false
                }
                Spacer()
                Button("Join", action: join)
                    .keyboardShortcut(.defaultAction)
                    .disabled(joinCode.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }
        .padding(10)
        .ohyaGlassControl(cornerRadius: 10)
    }

    private func join() {
        let code = joinCode
        joinCode = ""
        guard !code.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { return }
        joining = false
        Task { await model.runSetup(joinCode: code) }
    }

    private func choiceButton(title: String, detail: String, systemImage: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack(spacing: 10) {
                Image(systemName: systemImage)
                    .font(.title3)
                    .foregroundStyle(OhyaTheme.brandPurple)
                    .frame(width: 24)
                VStack(alignment: .leading, spacing: 2) {
                    Text(title)
                        .font(.subheadline.weight(.medium))
                    Text(detail)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                Spacer()
            }
            .padding(10)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .ohyaGlassControl(cornerRadius: 10)
        .disabled(model.resolvedBinaryPath == nil)
    }

    private func outcomeMark(_ outcome: SetupResult.Outcome) -> some View {
        let symbol: (name: String, color: Color) = switch outcome {
        case .success: ("checkmark.circle.fill", .green)
        case .neutral: ("circle", .secondary)
        case .warning: ("exclamationmark.triangle.fill", .orange)
        case .failure: ("xmark.circle.fill", .red)
        }
        return Image(systemName: symbol.name)
            .foregroundStyle(symbol.color)
            .font(.caption)
            .padding(.top, 2)
    }

    private func results(_ report: SetupReport) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(report.isComplete ? "Setup complete" : "Setup is incomplete")
                .font(.subheadline.weight(.medium))
            ForEach(report.results) { result in
                HStack(alignment: .top, spacing: 6) {
                    outcomeMark(result.outcome)
                    VStack(alignment: .leading, spacing: 1) {
                        Text("\(result.component): \(result.status)")
                            .font(.caption.weight(.medium))
                        if let detail = result.detail, !detail.isEmpty, result.outcome == .failure || result.outcome == .warning {
                            Text(detail)
                                .font(.caption2)
                                .foregroundStyle(.secondary)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                    }
                }
            }
            if !report.isComplete {
                Button("Run Setup Again") {
                    Task { await model.runSetup(joinCode: nil) }
                }
                .disabled(model.isRunningSetup)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(10)
        .ohyaGlassControl(cornerRadius: 10)
    }
}
