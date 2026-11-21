import OhyaMenuBarCore
import SwiftUI

struct RecallView: View {
    @Bindable var model: AppModel
    @FocusState private var searchFocused: Bool
    @State private var hoveredID: String?

    var body: some View {
        VStack(spacing: 8) {
            searchCapsule
                .padding(.horizontal, 14)
                .padding(.top, 10)

            agentFilterBar
                .padding(.horizontal, 14)

            if let error = model.recallError {
                Text(error)
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, 14)
            }

            ScrollView {
                LazyVStack(spacing: 4) {
                    if model.isLoadingRecall && displayedIsEmpty {
                        ProgressView()
                            .controlSize(.small)
                            .padding(.vertical, 24)
                    } else if model.searchQuery.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                        recentSessionsList
                    } else {
                        searchHitsList
                    }
                }
                .padding(.horizontal, 10)
                .padding(.bottom, 10)
            }
        }
        .onAppear {
            searchFocused = true
        }
    }

    private var displayedIsEmpty: Bool {
        model.searchQuery.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
            ? model.recentSessions.isEmpty
            : model.searchHits.isEmpty
    }

    private var searchCapsule: some View {
        HStack(spacing: 6) {
            Image(systemName: "magnifyingglass")
                .font(.caption)
                .foregroundStyle(.secondary)

            TextField("Search memory…", text: $model.searchQuery)
                .textFieldStyle(.plain)
                .font(.subheadline)
                .focused($searchFocused)
                .onChange(of: model.searchQuery) {
                    model.scheduleRecallSearch()
                }
                .onSubmit {
                    Task { await model.refreshRecall() }
                }

            if !model.searchQuery.isEmpty {
                Button {
                    model.searchQuery = ""
                    Task { await model.refreshRecall() }
                } label: {
                    Image(systemName: "xmark.circle.fill")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                .buttonStyle(.plain)
            }
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 7)
        .ohyaGlassControl(cornerRadius: 10)
    }

    private var agentFilterBar: some View {
        HStack(spacing: 6) {
            ForEach(RecallAgentFilter.allCases) { filter in
                let active = model.recallAgent == filter
                Button {
                    model.recallAgent = filter
                    Task { await model.refreshRecall() }
                } label: {
                    Text(filter.title)
                        .font(.caption2.weight(active ? .semibold : .regular))
                        .padding(.horizontal, 8)
                        .padding(.vertical, 3)
                        .foregroundStyle(active ? .primary : .secondary)
                        .background(
                            Capsule()
                                .fill(active ? Color.primary.opacity(0.12) : Color.primary.opacity(0.04))
                        )
                }
                .buttonStyle(.plain)
            }
            Spacer()
        }
    }

    @ViewBuilder
    private var recentSessionsList: some View {
        if model.recentSessions.isEmpty {
            emptyState("No recent sessions yet")
        } else {
            ForEach(model.recentSessions) { session in
                sessionRow(session)
            }
        }
    }

    @ViewBuilder
    private var searchHitsList: some View {
        if model.searchHits.isEmpty {
            emptyState("No matches for “\(model.searchQuery)”")
        } else {
            ForEach(model.searchHits) { hit in
                searchHitRow(hit)
            }
        }
    }

    private func sessionRow(_ session: SessionSummary) -> some View {
        let isExpanded = model.expandedSessionID == session.id
        let isHovered = hoveredID == session.id
        return VStack(alignment: .leading, spacing: 6) {
            HStack(alignment: .top, spacing: 8) {
                VStack(alignment: .leading, spacing: 2) {
                    HStack(spacing: 5) {
                        Text(HandoffPromptBuilder.projectDisplayName(repo: session.repo, cwd: session.cwd))
                            .font(.subheadline.weight(.medium))
                            .lineLimit(1)
                        if session.settled == false {
                            Text("active")
                                .font(.caption2.weight(.medium))
                                .foregroundStyle(OhyaTheme.brandPurple)
                                .padding(.horizontal, 5)
                                .padding(.vertical, 1)
                                .background(Capsule().fill(OhyaTheme.brandPurple.opacity(0.14)))
                        }
                    }

                    HStack(spacing: 4) {
                        Text(DisplayFormat.agentDisplayName(session.agent))
                        Text("·")
                        Text(DisplayFormat.shortID(session.deviceID))
                        if let ms = session.lastEventAtMS ?? session.startedAtMS {
                            Text("·")
                            Text(DisplayFormat.relativeAge(fromEpochMS: ms))
                        }
                    }
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)

                    if let prompt = session.firstPrompt, !prompt.isEmpty {
                        Text(prompt)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .lineLimit(2)
                            .padding(.top, 1)
                    }
                }
                Spacer(minLength: 4)
                CopyFeedbackButton("Handoff", copiedLabel: "Copied") {
                    model.copyHandoffPrompt(for: session)
                }
            }
            .contentShape(Rectangle())
            .onTapGesture {
                Task { await model.toggleSessionPreview(session.id) }
            }

            if isExpanded {
                inlinePreview
            }
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 7)
        .ohyaRowHighlight(hovered: isHovered, selected: isExpanded)
        .onHover { inside in
            hoveredID = inside ? session.id : nil
        }
    }

    private func searchHitRow(_ hit: SearchHit) -> some View {
        let key = "hit-\(hit.eventID)"
        let isExpanded = model.expandedSessionID == hit.sessionID
        let isHovered = hoveredID == key
        return VStack(alignment: .leading, spacing: 6) {
            HStack(alignment: .top, spacing: 8) {
                VStack(alignment: .leading, spacing: 2) {
                    HStack(spacing: 4) {
                        Text(HandoffPromptBuilder.projectDisplayName(repo: hit.repo, cwd: nil))
                            .font(.subheadline.weight(.medium))
                            .lineLimit(1)
                        Text("·")
                            .foregroundStyle(.secondary)
                        Text(DisplayFormat.agentDisplayName(hit.agent))
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                        if let ms = hit.timeMS {
                            Text("·")
                                .foregroundStyle(.secondary)
                            Text(DisplayFormat.relativeAge(fromEpochMS: ms))
                                .font(.caption2)
                                .foregroundStyle(.secondary)
                        }
                    }
                    Text(hit.excerpt)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(3)
                }
                Spacer(minLength: 4)
                CopyFeedbackButton("Handoff", copiedLabel: "Copied") {
                    model.copyHandoffPrompt(for: hit)
                }
            }
            .contentShape(Rectangle())
            .onTapGesture {
                Task { await model.toggleSessionPreview(hit.sessionID) }
            }

            if isExpanded {
                inlinePreview
            }
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 7)
        .ohyaRowHighlight(hovered: isHovered, selected: isExpanded)
        .onHover { inside in
            hoveredID = inside ? key : nil
        }
    }

    @ViewBuilder
    private var inlinePreview: some View {
        VStack(alignment: .leading, spacing: 4) {
            Divider().opacity(0.35)
            if model.isLoadingPreview {
                ProgressView()
                    .controlSize(.mini)
                    .padding(.vertical, 4)
            } else if let show = model.expandedShowData {
                if show.events.isEmpty {
                    Text("No events stored.")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                } else {
                    ForEach(show.events.suffix(4)) { event in
                        VStack(alignment: .leading, spacing: 1) {
                            Text("\(event.role ?? event.kind)\(event.toolName.map { ":\($0)" } ?? "")")
                                .font(.caption2.weight(.semibold))
                                .foregroundStyle(.tertiary)
                            if !event.excerpt.isEmpty {
                                Text(event.excerpt)
                                    .font(.caption2)
                                    .foregroundStyle(.secondary)
                                    .lineLimit(2)
                            }
                        }
                    }
                }
            }
        }
        .padding(.top, 2)
    }

    private func emptyState(_ text: String) -> some View {
        Text(text)
            .font(.caption)
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity)
            .padding(.vertical, 28)
    }
}
