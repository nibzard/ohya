import OhyaMenuBarCore
import SwiftUI

struct SettingsView: View {
    @Bindable var model: AppModel

    var body: some View {
        Form {
            Section("General") {
                Toggle("Launch ohya menu bar at login", isOn: Binding(
                    get: { model.loginItem.isEnabled },
                    set: { model.loginItem.setEnabled($0) }
                ))

                if model.loginItem.requiresApproval {
                    Text("Approve ohya in System Settings → General → Login Items.")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }

                if let error = model.loginItem.lastError {
                    Text(error)
                        .font(.caption)
                        .foregroundStyle(.red)
                        .textSelection(.enabled)
                }

                Toggle("Notify on stopped sources or unexpected downtime", isOn: $model.notificationsEnabled)
            }

            Section("ohya CLI") {
                TextField("Executable path (leave empty for automatic lookup)", text: $model.binaryOverridePath)
                    .textFieldStyle(.roundedBorder)

                LabeledContent("Resolved binary") {
                    Text(model.resolvedBinaryPath ?? "Not found")
                        .font(.caption.monospaced())
                        .foregroundStyle(model.resolvedBinaryPath == nil ? .red : .secondary)
                        .textSelection(.enabled)
                }

                if let version = model.version {
                    LabeledContent("Version") {
                        Text("\(version.version) (schema \(version.schema))")
                            .font(.caption.monospacedDigit())
                            .foregroundStyle(.secondary)
                            .textSelection(.enabled)
                    }
                }
            }
        }
        .formStyle(.grouped)
        .frame(width: 440)
        .fixedSize(horizontal: false, vertical: true)
        .onAppear {
            model.loginItem.refresh()
        }
    }
}
