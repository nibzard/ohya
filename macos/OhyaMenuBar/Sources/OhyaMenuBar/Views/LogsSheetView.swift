import OhyaMenuBarCore
import SwiftUI

struct LogsSheetView: View {
    let model: AppModel
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("Recent Warnings & Errors")
                    .font(.headline)
                Spacer()
                if let entries = model.logsData?.entries, !entries.isEmpty {
                    CopyFeedbackButton("Copy All") {
                        model.copyText(CopyFormatter.logs(entries.reversed()))
                    }
                }
                Button("Done") { dismiss() }
                    .keyboardShortcut(.cancelAction)
            }

            Divider().opacity(0.45)

            if model.isLoadingLogs {
                ProgressView()
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if let entries = model.logsData?.entries, !entries.isEmpty {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 6) {
                        ForEach(entries.reversed()) { entry in
                            VStack(alignment: .leading, spacing: 2) {
                                HStack(spacing: 6) {
                                    Text(entry.level.uppercased())
                                        .font(.caption2.weight(.semibold))
                                        .foregroundStyle(entry.level == "error" ? .red : .orange)
                                    Text(entry.component)
                                        .font(.caption2)
                                        .foregroundStyle(.secondary)
                                    Spacer()
                                    Text(DisplayFormat.relativeAge(fromEpochMS: entry.timeMS))
                                        .font(.caption2.monospacedDigit())
                                        .foregroundStyle(.tertiary)
                                }
                                Text(entry.eventCode + (entry.outcome.map { " (\($0))" } ?? ""))
                                    .font(.caption.monospaced())
                                    .foregroundStyle(.primary)
                            }
                            .padding(.vertical, 4)
                            Divider().opacity(0.3)
                        }
                    }
                    .textSelection(.enabled)
                }
            } else {
                Text("No warnings or errors in the last 24 hours.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }
        .padding(14)
        .frame(width: 340, height: 320)
    }
}
