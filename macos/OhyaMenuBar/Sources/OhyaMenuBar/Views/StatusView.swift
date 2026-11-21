import OhyaMenuBarCore
import SwiftUI

struct StatusView: View {
    let model: AppModel
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                if let error = model.lastError {
                    unavailableCard(error)
                }
                if model.lastError == nil || model.status != nil {
                    if model.summary.totalBacklogBytes > 0 {
                        backlogCard
                    }
                    if !actionableFindings.isEmpty {
                        findingsSection
                    }
                    if let purge = model.purgePlan, !purge.sessions.isEmpty {
                        cleanupBanner(purge)
                    }
                    agentsSection
                    overviewSection
                }
            }
            .padding(14)
        }
    }

    private var actionableFindings: [StatusFinding] {
        model.summary.warnings.filter {
            $0.code != "pending_backlog" && $0.code != "upload_backlog"
        }
    }

    private func unavailableCard(_ error: OhyaRunnerError) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Label(error.localizedDescription, systemImage: "exclamationmark.triangle.fill")
                .font(.subheadline.weight(.medium))
                .foregroundStyle(.orange)
            if let hint = error.hintText {
                Text(hint)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(10)
        .ohyaGlassControl(cornerRadius: 10)
    }

    private var backlogCard: some View {
        HStack(spacing: 10) {
            Image(systemName: model.summary.isRemote ? "arrow.up.circle.fill" : "arrow.down.circle.fill")
                .foregroundStyle(OhyaTheme.brandPurple)
                .font(.title3)
            VStack(alignment: .leading, spacing: 2) {
                Text(model.summary.isRemote ? "Uploading sessions" : "Importing history")
                    .font(.subheadline.weight(.medium))
                Text("\(DisplayFormat.bytes(model.summary.totalBacklogBytes)) remaining")
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
                    .contentTransition(reduceMotion ? .identity : .numericText())
            }
            Spacer()
        }
        .padding(10)
        .ohyaGlassControl(cornerRadius: 10)
    }

    private var findingsSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(actionableFindings) { finding in
                VStack(alignment: .leading, spacing: 6) {
                    HStack(alignment: .top, spacing: 6) {
                        Image(systemName: finding.severity == "error" ? "xmark.octagon.fill" : "exclamationmark.triangle.fill")
                            .foregroundStyle(finding.severity == "error" ? .red : .orange)
                            .font(.caption)
                            .padding(.top, 2)
                        VStack(alignment: .leading, spacing: 2) {
                            Text(finding.evidence)
                                .font(.caption)
                                .foregroundStyle(.primary)
                                .fixedSize(horizontal: false, vertical: true)
                            if let hint = finding.hint, !hint.isEmpty {
                                Text(hint)
                                    .font(.caption2)
                                    .foregroundStyle(.secondary)
                                    .fixedSize(horizontal: false, vertical: true)
                            }
                        }
                    }
                    if let hint = finding.hint, hint.contains("ohya ") {
                        HStack {
                            Spacer()
                            CopyFeedbackButton("Copy fix", systemImage: "terminal") {
                                model.clipboard.copyText(hint)
                            }
                        }
                    }
                }
                .padding(10)
                .background(
                    RoundedRectangle(cornerRadius: 10, style: .continuous)
                        .fill(Color.orange.opacity(0.08))
                )
                .overlay(
                    RoundedRectangle(cornerRadius: 10, style: .continuous)
                        .strokeBorder(Color.orange.opacity(0.22), lineWidth: 0.75)
                )
            }
        }
    }

    private func cleanupBanner(_ plan: PurgeData) -> some View {
        HStack(spacing: 8) {
            Image(systemName: "sparkles")
                .foregroundStyle(OhyaTheme.brandPurple)
                .font(.caption)
            VStack(alignment: .leading, spacing: 1) {
                Text("\(plan.sessions.count) sessions marked for cleanup")
                    .font(.caption.weight(.medium))
                Text("\(DisplayFormat.bytes(Double(plan.bytes))) nominated by agents")
                    .font(.caption2.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Button("Review") {
                model.showingPurgeReview = true
            }
            .buttonStyle(.bordered)
            .controlSize(.small)
        }
        .padding(10)
        .ohyaGlassControl(cornerRadius: 10)
    }

    private var agentsSection: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Agents")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)

            VStack(spacing: 0) {
                let roots = model.status?.topology.agentRoots ?? []
                ForEach(Array(roots.enumerated()), id: \.element.id) { index, root in
                    HStack {
                        Circle()
                            .fill(root.present > 0 ? Color.green.opacity(0.85) : Color.secondary.opacity(0.35))
                            .frame(width: 6, height: 6)
                        Text(DisplayFormat.agentDisplayName(root.agent))
                            .font(.subheadline)
                        Spacer()
                        Text(agentRightLabel(root))
                            .font(.caption.monospacedDigit())
                            .foregroundStyle(.secondary)
                    }
                    .padding(.vertical, 6)
                    if index < roots.count - 1 {
                        Divider().opacity(0.35)
                    }
                }
            }
        }
    }

    private var overviewSection: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Overview")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)

            VStack(spacing: 6) {
                if model.summary.activeSources > 0 {
                    infoRow("Sources", value: "\(model.summary.activeSources) active")
                }

                infoRow("Storage", value: storageSummaryText)

                if let remoteHub = model.status?.remoteHub, remoteHub.reachable, let hub = remoteHub.hub {
                    infoRow("Hub", value: "\(hub.version) · \(remoteHub.transportPath ?? "connected")")
                }

                if let recallText = usageSummaryText {
                    infoRow("Recall (24h)", value: recallText)
                }

                if let sampleText = sampleFreshnessText {
                    infoRow("Last sample", value: sampleText)
                }

                if let doctorSummary = doctorSummaryText {
                    infoRow("Health", value: doctorSummary)
                }
            }
        }
    }

    private func infoRow(_ title: String, value: String) -> some View {
        HStack {
            Text(title)
                .font(.caption)
                .foregroundStyle(.secondary)
            Spacer()
            Text(value)
                .font(.caption.monospacedDigit())
                .foregroundStyle(.primary)
                .lineLimit(1)
        }
    }

    private func agentRightLabel(_ root: StatusAgentRoot) -> String {
        let excluded = excludedCount(for: root.agent)
        if root.present == 0 {
            return "Not detected"
        }
        if excluded > 0 {
            return "Watching · \(excluded) excl"
        }
        return "Watching"
    }

    private func excludedCount(for agent: String) -> Int {
        guard let instances = model.status?.instances else { return 0 }
        return instances.reduce(0) { sum, inst in
            sum + Int(inst.excluded?[agent] ?? 0)
        }
    }

    private var storageSummaryText: String {
        guard let storage = model.status?.storage else { return "—" }
        if storage.location == "remote-hub" {
            if let remote = model.status?.remoteHub, remote.reachable, remote.supported, let files = remote.storageFiles {
                let db = Double(files["db"] ?? 0)
                let wal = Double(files["wal"] ?? 0)
                if db > 0 || wal > 0 {
                    if wal >= 1024 * 1024 {
                        return "\(DisplayFormat.bytes(db)) (+\(DisplayFormat.bytes(wal)) WAL) · hub"
                    }
                    return "\(DisplayFormat.bytes(db)) · hub"
                }
            }
            return "Owned by hub"
        }
        let db = Double(storage.files?["db"] ?? 0)
        let wal = Double(storage.files?["wal"] ?? 0)
        if db == 0 && wal == 0 {
            return "Empty"
        }
        if wal >= 1024 * 1024 {
            return "\(DisplayFormat.bytes(db)) (+\(DisplayFormat.bytes(wal)) WAL)"
        }
        return DisplayFormat.bytes(db)
    }

    private var usageSummaryText: String? {
        guard let usage = model.usage else { return nil }
        let total = max(usage.transcript.calls, usage.server.records)
        if total == 0 { return "0 tool calls" }
        return "\(total) tool calls"
    }

    private var sampleFreshnessText: String? {
        guard let newest = model.status?.instances.compactMap(\.sampledWallMS).max() else {
            return nil
        }
        return DisplayFormat.relativeAge(fromEpochMS: newest)
    }

    private var doctorSummaryText: String? {
        guard let doctor = model.doctor else { return nil }
        let errs = doctor.findings.filter { $0.severity == "error" }.count
        let warns = doctor.findings.filter { $0.severity == "warn" }.count
        if errs > 0 { return "\(errs) error\(errs == 1 ? "" : "s")" }
        if warns > 0 { return "\(warns) warning\(warns == 1 ? "" : "s")" }
        return "All checks ok"
    }
}
