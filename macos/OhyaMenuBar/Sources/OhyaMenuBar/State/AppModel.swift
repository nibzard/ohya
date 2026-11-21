import AppKit
import Foundation
import Observation
import OhyaMenuBarCore

enum PopoverSection: String, CaseIterable, Identifiable {
    case status
    case recall
    case devices

    var id: String { rawValue }

    var title: String {
        switch self {
        case .status: return "Status"
        case .recall: return "Recall"
        case .devices: return "Devices"
        }
    }
}

enum RecallAgentFilter: String, CaseIterable, Identifiable {
    case all = ""
    case claude = "claude-code"
    case codex = "codex"
    case pi = "pi"

    var id: String { rawValue }

    var title: String {
        switch self {
        case .all: return "All"
        case .claude: return "Claude"
        case .codex: return "Codex"
        case .pi: return "Pi"
        }
    }
}

@MainActor
@Observable
final class AppModel {
    private static let binaryOverrideKey = "ohya.binaryOverridePath"
    private static let notificationsEnabledKey = "ohya.notificationsEnabled"

    let clipboard = ClipboardGuard()
    let loginItem = LoginItemStore()
    private let runner = OhyaRunner()
    private let watcher = ObserveWatcher()
    private let notifier = Notifier()

    var selectedSection: PopoverSection = .status
    var isPopoverVisible = false

    var binaryOverridePath: String {
        didSet {
            UserDefaults.standard.set(binaryOverridePath, forKey: Self.binaryOverrideKey)
            binaryOverrideTask?.cancel()
            binaryOverrideTask = Task { [weak self] in
                try? await Task.sleep(for: .milliseconds(600))
                guard !Task.isCancelled else { return }
                await self?.refreshAll()
            }
        }
    }

    var notificationsEnabled: Bool {
        didSet {
            UserDefaults.standard.set(notificationsEnabled, forKey: Self.notificationsEnabledKey)
            if notificationsEnabled {
                Task { _ = await notifier.requestAuthorizationIfNeeded() }
            }
        }
    }

    private(set) var resolvedBinaryPath: String?
    private(set) var version: VersionData?
    private(set) var status: StatusData?
    private(set) var serviceStatus: ServiceStatusData?
    private(set) var doctor: DoctorData?
    private(set) var usage: UsageData?
    private(set) var purgePlan: PurgeData?
    private(set) var lastError: OhyaRunnerError?
    private(set) var summary = StatusEvaluator.evaluate(status: nil, service: nil, error: nil)

    private(set) var isRefreshing = false
    private var statusRefreshPending = false
    private(set) var isTogglingService = false
    private(set) var isExportingDiagnostics = false
    private(set) var transientMessage: String?

    var searchQuery = ""
    var recallAgent: RecallAgentFilter = .all
    private(set) var recentSessions: [SessionSummary] = []
    private(set) var searchHits: [SearchHit] = []
    private(set) var isLoadingRecall = false
    private var recallGeneration = 0
    private(set) var recallError: String?
    var expandedSessionID: String?
    private(set) var expandedShowData: ShowData?
    private(set) var isLoadingPreview = false

    private(set) var devicesData: DevicesListData?
    private(set) var activeInvite: DeviceInviteData?
    private(set) var isLoadingDevices = false
    private(set) var devicesError: String?

    private(set) var logsData: LogsData?
    private(set) var isLoadingLogs = false
    var showingLogs = false
    var showingPurgeReview = false

    private var pollTask: Task<Void, Never>?
    private var searchTask: Task<Void, Never>?
    private var binaryOverrideTask: Task<Void, Never>?
    private var messageTask: Task<Void, Never>?
    private var lastSecondaryFetch: Date = .distantPast
    private var lastRemoteHubFetch: Date = .distantPast
    private var previousSummary: StatusSummary?

    init() {
        self.binaryOverridePath = UserDefaults.standard.string(forKey: Self.binaryOverrideKey) ?? ""
        self.notificationsEnabled = UserDefaults.standard.bool(forKey: Self.notificationsEnabledKey)
        startLoop()
    }

    func startLoop() {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            guard let self else { return }
            await self.refreshAll()
            while !Task.isCancelled {
                let delay: Duration = self.isPopoverVisible ? .seconds(4) : .seconds(15)
                try? await Task.sleep(for: delay)
                guard !Task.isCancelled else { break }
                await self.refreshStatus()
            }
        }
    }

    func setPopoverVisible(_ visible: Bool) {
        isPopoverVisible = visible
        guard visible else { return }
        Task {
            await refreshStatus()
            await refreshSecondaryIfStale()
            if selectedSection == .recall {
                await refreshRecall()
            } else if selectedSection == .devices {
                await refreshDevices()
            }
        }
    }

    func selectSection(_ section: PopoverSection) {
        selectedSection = section
        switch section {
        case .status:
            Task { await refreshSecondaryIfStale() }
        case .recall:
            Task { await refreshRecall() }
        case .devices:
            Task { await refreshDevices() }
        }
    }

    func refreshAll() async {
        resolveBinary()
        await refreshVersion()
        await refreshStatus()
        await refreshSecondary(force: true)
    }

    private func resolveBinary() {
        switch BinaryLocator.resolve(overridePath: binaryOverridePath) {
        case .success(let path):
            resolvedBinaryPath = path
            if case .binaryNotFound = lastError {
                lastError = nil
            }
        case .failure(let err):
            resolvedBinaryPath = nil
            lastError = err
            updateSummary()
        }
    }

    private func refreshVersion() async {
        guard let binary = resolvedBinaryPath else { return }
        if let response: CLIResponse<VersionData> = try? await runner.runJSON(
            binaryPath: binary,
            arguments: ["version", "--json"],
            timeout: 5
        ) {
            version = response.data
        }
    }

    func refreshStatus() async {
        if isRefreshing {
            statusRefreshPending = true
            return
        }
        isRefreshing = true
        defer { isRefreshing = false }
        repeat {
            statusRefreshPending = false
            await performStatusRefresh()
        } while statusRefreshPending
    }

    private func performStatusRefresh() async {
        if resolvedBinaryPath == nil {
            resolveBinary()
        }
        guard let binary = resolvedBinaryPath else { return }

        do {
            var statusArgs = ["status", "--json"]
            let fetchRemote = status?.topology.mode == "remote" && Date().timeIntervalSince(lastRemoteHubFetch) >= 15
            if fetchRemote {
                statusArgs.append("--remote")
                lastRemoteHubFetch = Date()
            }
            let statusResp: CLIResponse<StatusData> = try await runner.runJSON(
                binaryPath: binary,
                arguments: statusArgs,
                timeout: fetchRemote ? 18 : 10,
                acceptDegradedExit3: true
            )
            var next = statusResp.data
            if !fetchRemote, next.remoteHub == nil {
                next.remoteHub = status?.remoteHub
            }
            status = next
            lastError = nil

            let observeDir = (statusResp.data.topology.stateDir as NSString).appendingPathComponent("observe")
            watcher.watch(directory: observeDir) { [weak self] in
                Task { @MainActor [weak self] in
                    await self?.refreshStatus()
                }
            }
        } catch let err as OhyaRunnerError {
            lastError = err
        } catch {
            lastError = .processLaunchFailed(error.localizedDescription)
        }

        if let serviceResp: CLIResponse<ServiceStatusData> = try? await runner.runJSON(
            binaryPath: binary,
            arguments: ["service", "status", "--json"],
            timeout: 8
        ) {
            serviceStatus = serviceResp.data
        }

        updateSummary()
    }

    private func refreshSecondaryIfStale() async {
        if Date().timeIntervalSince(lastSecondaryFetch) > 60 {
            await refreshSecondary(force: false)
        }
    }

    func refreshSecondary(force: Bool) async {
        guard let binary = resolvedBinaryPath else { return }
        if !force && Date().timeIntervalSince(lastSecondaryFetch) <= 60 {
            return
        }
        lastSecondaryFetch = Date()

        if let usageResp: CLIResponse<UsageData> = try? await runner.runJSON(
            binaryPath: binary,
            arguments: ["usage", "--since", "24h", "--json"],
            timeout: 12
        ) {
            usage = usageResp.data
        }

        if let doctorResp: CLIResponse<DoctorData> = try? await runner.runJSON(
            binaryPath: binary,
            arguments: ["doctor", "--json"],
            timeout: 15
        ) {
            doctor = doctorResp.data
        }

        if !summary.isRemote {
            if let purgeResp: CLIResponse<PurgeData> = try? await runner.runJSON(
                binaryPath: binary,
                arguments: ["purge", "--marked", "tags.garbage", "--json"],
                timeout: 10
            ) {
                purgePlan = purgeResp.data
            }
        } else {
            purgePlan = nil
        }
    }

    func toggleService() async {
        guard let binary = resolvedBinaryPath, !isTogglingService else { return }
        isTogglingService = true
        defer { isTogglingService = false }

        let action = summary.isPaused ? "start" : "stop"
        do {
            let resp: CLIResponse<ServiceStatusData> = try await runner.runJSON(
                binaryPath: binary,
                arguments: ["service", action, "--json"],
                timeout: 20
            )
            serviceStatus = resp.data
            showTransientMessage(resp.data.running ? "Capture resumed" : "Capture paused")
            await refreshStatus()
        } catch let err as OhyaRunnerError {
            showTransientMessage(err.localizedDescription)
        } catch {
            showTransientMessage(error.localizedDescription)
        }
    }

    func scheduleRecallSearch() {
        searchTask?.cancel()
        searchTask = Task { [weak self] in
            try? await Task.sleep(for: .milliseconds(220))
            guard !Task.isCancelled else { return }
            await self?.refreshRecall()
        }
    }

    func refreshRecall() async {
        guard let binary = resolvedBinaryPath else { return }
        recallGeneration += 1
        let generation = recallGeneration
        isLoadingRecall = true
        recallError = nil

        let formatted = FTSQueryFormatter.format(searchQuery)
        var agentArgs: [String] = []
        if recallAgent != .all {
            agentArgs = ["--agent", recallAgent.rawValue]
        }
        do {
            if formatted.isEmpty {
                let resp: CLIResponse<SessionsData> = try await runner.runJSON(
                    binaryPath: binary,
                    arguments: ["sessions", "--limit", "12", "--json"] + agentArgs,
                    timeout: 10
                )
                guard generation == recallGeneration else { return }
                recentSessions = resp.data.sessions
                searchHits = []
            } else {
                let resp: CLIResponse<SearchData> = try await runner.runJSON(
                    binaryPath: binary,
                    arguments: ["search", formatted, "--json"] + agentArgs,
                    timeout: 10
                )
                guard generation == recallGeneration else { return }
                searchHits = resp.data.matches
            }
        } catch {
            guard generation == recallGeneration else { return }
            recallError = error.localizedDescription
        }
        isLoadingRecall = false
    }

    func toggleSessionPreview(_ sessionID: String) async {
        if expandedSessionID == sessionID {
            expandedSessionID = nil
            expandedShowData = nil
            return
        }
        guard let binary = resolvedBinaryPath else { return }
        expandedSessionID = sessionID
        expandedShowData = nil
        isLoadingPreview = true
        defer { isLoadingPreview = false }

        if let resp: CLIResponse<ShowData> = try? await runner.runJSON(
            binaryPath: binary,
            arguments: ["show", sessionID, "--last", "6", "--json"],
            timeout: 10
        ) {
            if expandedSessionID == sessionID {
                expandedShowData = resp.data
            }
        }
    }

    func copyHandoffPrompt(for session: SessionSummary) {
        clipboard.copyText(HandoffPromptBuilder.build(for: session))
    }

    func copyHandoffPrompt(for hit: SearchHit) {
        clipboard.copyText(HandoffPromptBuilder.build(for: hit))
    }

    func refreshDevices() async {
        guard let binary = resolvedBinaryPath else { return }
        if summary.isRemote {
            await refreshSecondary(force: true)
            return
        }
        isLoadingDevices = true
        devicesError = nil
        defer { isLoadingDevices = false }

        do {
            let resp: CLIResponse<DevicesListData> = try await runner.runJSON(
                binaryPath: binary,
                arguments: ["devices", "list", "--json"],
                timeout: 10
            )
            devicesData = resp.data
        } catch let err as OhyaRunnerError {
            devicesError = err.localizedDescription
        } catch {
            devicesError = error.localizedDescription
        }
    }

    func createDeviceInvite() async {
        guard let binary = resolvedBinaryPath else { return }
        devicesError = nil
        do {
            let resp: CLIResponse<DeviceInviteData> = try await runner.runJSON(
                binaryPath: binary,
                arguments: ["devices", "invite", "--json"],
                timeout: 10
            )
            activeInvite = resp.data
            await refreshDevices()
        } catch let err as OhyaRunnerError {
            devicesError = err.localizedDescription
        } catch {
            devicesError = error.localizedDescription
        }
    }

    func copyInviteCode() {
        guard let invite = activeInvite else { return }
        clipboard.copySecret(invite.code)
        showTransientMessage("Join code copied (auto-clears in 60s)")
    }

    func copyJoinCommand() {
        clipboard.copyText("curl -fsSL https://github.com/nibzard/ohya/releases/latest/download/install.sh | sh -s -- --join")
        showTransientMessage("Join command copied")
    }

    func revokeDevice(_ deviceID: String) async {
        guard let binary = resolvedBinaryPath else { return }
        devicesError = nil
        do {
            let _: CLIResponse<DeviceRevokeData> = try await runner.runJSON(
                binaryPath: binary,
                arguments: ["devices", "revoke", deviceID, "--json"],
                timeout: 10
            )
            showTransientMessage("Revoked \(DisplayFormat.shortID(deviceID))")
            await refreshDevices()
        } catch let err as OhyaRunnerError {
            devicesError = err.localizedDescription
        } catch {
            devicesError = error.localizedDescription
        }
    }

    func applyMarkedPurge() async {
        guard let binary = resolvedBinaryPath else { return }
        do {
            let resp: CLIResponse<PurgeData> = try await runner.runJSON(
                binaryPath: binary,
                arguments: ["purge", "--marked", "tags.garbage", "--apply", "--json"],
                timeout: 30
            )
            purgePlan = nil
            showingPurgeReview = false
            showTransientMessage("Purged \(resp.data.sessions.count) sessions")
            await refreshStatus()
            await refreshSecondary(force: true)
        } catch let err as OhyaRunnerError {
            showTransientMessage(err.localizedDescription)
        } catch {
            showTransientMessage(error.localizedDescription)
        }
    }

    func openLogs() async {
        showingLogs = true
        guard let binary = resolvedBinaryPath else { return }
        isLoadingLogs = true
        defer { isLoadingLogs = false }
        if let resp: CLIResponse<LogsData> = try? await runner.runJSON(
            binaryPath: binary,
            arguments: ["logs", "--since", "24h", "--level", "warn", "--limit", "30", "--json"],
            timeout: 10,
            acceptDegradedExit3: true
        ) {
            logsData = resp.data
        }
    }

    func exportDiagnostics() async {
        guard let binary = resolvedBinaryPath, !isExportingDiagnostics else { return }
        NSApp.activate(ignoringOtherApps: true)
        let panel = NSSavePanel()
        panel.title = "Export ohya Diagnostics"
        panel.nameFieldStringValue = "ohya-diagnostics.json"
        panel.canCreateDirectories = true
        guard panel.runModal() == .OK, let destination = panel.url else { return }

        isExportingDiagnostics = true
        defer { isExportingDiagnostics = false }

        do {
            let result = try await runner.runRaw(
                binaryPath: binary,
                arguments: ["diagnose", "--force", "--output", destination.path],
                timeout: 30
            )
            if result.exitCode == 0 || result.exitCode == 3 {
                NSWorkspace.shared.activateFileViewerSelecting([destination])
                showTransientMessage("Diagnostics saved")
            } else {
                showTransientMessage("Diagnostics export failed")
            }
        } catch {
            showTransientMessage(error.localizedDescription)
        }
    }

    func showTransientMessage(_ message: String) {
        messageTask?.cancel()
        transientMessage = message
        messageTask = Task { [weak self] in
            try? await Task.sleep(for: .seconds(2.5))
            guard !Task.isCancelled else { return }
            self?.transientMessage = nil
        }
    }

    private func updateSummary() {
        let next = StatusEvaluator.evaluate(status: status, service: serviceStatus, error: lastError)
        if let alert = StatusEvaluator.transitionAlert(previous: previousSummary, current: next) {
            notifier.send(alert, enabled: notificationsEnabled)
        }
        previousSummary = next
        summary = next
    }
}
