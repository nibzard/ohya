import OhyaMenuBarCore
import SwiftUI

struct PurgeReviewSheet: View {
    let model: AppModel
    @Environment(\.dismiss) private var dismiss
    @State private var applying = false

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("Review Agent-Marked Cleanup")
                    .font(.headline)
                Spacer()
                Button("Cancel") { dismiss() }
                    .keyboardShortcut(.cancelAction)
            }

            Text("Agents nominated these sessions under tags.garbage. Purging deletes whole sessions and tombstones their sources so purged bytes never return automatically.")
                .font(.caption)
                .foregroundStyle(.secondary)

            Divider().opacity(0.45)

            if let plan = model.purgePlan {
                HStack {
                    Text("\(plan.sessions.count) sessions · \(plan.events) events · \(DisplayFormat.bytes(Double(plan.bytes)))")
                        .font(.caption.monospacedDigit().weight(.medium))
                    Spacer()
                }

                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 6) {
                        ForEach(plan.sessions) { session in
                            HStack {
                                VStack(alignment: .leading, spacing: 1) {
                                    Text(session.id)
                                        .font(.caption.monospaced())
                                        .lineLimit(1)
                                    Text("\(DisplayFormat.agentDisplayName(session.agent)) · \(session.events) events")
                                        .font(.caption2)
                                        .foregroundStyle(.secondary)
                                }
                                Spacer()
                            }
                            Divider().opacity(0.3)
                        }
                    }
                }

                HStack {
                    Spacer()
                    Button(role: .destructive) {
                        applying = true
                        Task {
                            await model.applyMarkedPurge()
                            applying = false
                        }
                    } label: {
                        Text(applying ? "Purging…" : "Purge \(plan.sessions.count) Sessions")
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(.red)
                    .disabled(applying || plan.sessions.isEmpty)
                }
            }
        }
        .padding(14)
        .frame(width: 360, height: 340)
    }
}
