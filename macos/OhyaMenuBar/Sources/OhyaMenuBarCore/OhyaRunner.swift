import Foundation

public enum OhyaRunnerError: Error, Sendable, Equatable {
    case binaryNotFound([String])
    case timedOut(command: [String], seconds: TimeInterval)
    case processLaunchFailed(String)
    case commandFailed(code: String, message: String, hint: String, exitCode: Int32)
    case decodeFailed(String)
}

extension OhyaRunnerError: LocalizedError {
    public var errorDescription: String? {
        switch self {
        case .binaryNotFound:
            return "Could not find the ohya executable."
        case .timedOut(_, let seconds):
            return "ohya timed out after \(Int(seconds))s."
        case .processLaunchFailed(let message):
            return message
        case .commandFailed(_, let message, _, _):
            return message
        case .decodeFailed(let message):
            return "Unexpected ohya output: \(message)"
        }
    }

    public var hintText: String? {
        switch self {
        case .binaryNotFound:
            return "Install ohya or set its path in Settings."
        case .commandFailed(_, _, let hint, _):
            return hint.isEmpty ? nil : hint
        default:
            return nil
        }
    }
}

public struct CLIResponse<T: Sendable>: Sendable {
    public let data: T
    public let degraded: Bool
    public let exitCode: Int32
}

public struct BinaryLocator: Sendable {
    public static func defaultCandidates(home: String = NSHomeDirectory(), bundleExecutableDir: String? = Bundle.main.executableURL?.deletingLastPathComponent().path) -> [String] {
        var candidates: [String] = []
        if let bundleDir = bundleExecutableDir {
            candidates.append((bundleDir as NSString).appendingPathComponent("ohya"))
        }
        candidates.append((home as NSString).appendingPathComponent(".local/bin/ohya"))
        candidates.append("/opt/homebrew/bin/ohya")
        candidates.append("/usr/local/bin/ohya")
        return candidates
    }

    public static func resolve(overridePath: String? = nil, fileManager: FileManager = .default, home: String = NSHomeDirectory(), bundleExecutableDir: String? = Bundle.main.executableURL?.deletingLastPathComponent().path) -> Result<String, OhyaRunnerError> {
        var searched: [String] = []
        if let overridePath {
            let trimmed = overridePath.trimmingCharacters(in: .whitespacesAndNewlines)
            if !trimmed.isEmpty {
                let expanded = (trimmed as NSString).expandingTildeInPath
                searched.append(expanded)
                if fileManager.isExecutableFile(atPath: expanded) {
                    return .success(expanded)
                }
                return .failure(.binaryNotFound(searched))
            }
        }
        for candidate in defaultCandidates(home: home, bundleExecutableDir: bundleExecutableDir) {
            searched.append(candidate)
            if fileManager.isExecutableFile(atPath: candidate) {
                return .success(candidate)
            }
        }
        return .failure(.binaryNotFound(searched))
    }
}

public actor OhyaRunner {
    private let decoder: JSONDecoder

    public init() {
        self.decoder = JSONDecoder()
    }

    public func runJSON<T: Decodable & Sendable>(
        binaryPath: String,
        arguments: [String],
        timeout: TimeInterval = 15,
        acceptDegradedExit3: Bool = false
    ) async throws -> CLIResponse<T> {
        let raw = try await execute(binaryPath: binaryPath, arguments: arguments, timeout: timeout)
        let allowed = raw.exitCode == 0 || (acceptDegradedExit3 && raw.exitCode == 3)

        let envelope: JSONEnvelope<T>
        do {
            envelope = try decoder.decode(JSONEnvelope<T>.self, from: raw.stdout)
        } catch {
            if !allowed {
                let stderrText = String(data: raw.stderr, encoding: .utf8)?
                    .trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                throw OhyaRunnerError.commandFailed(
                    code: "cli_error",
                    message: stderrText.isEmpty ? "ohya exited with status \(raw.exitCode)" : stderrText,
                    hint: "",
                    exitCode: raw.exitCode
                )
            }
            throw OhyaRunnerError.decodeFailed(error.localizedDescription)
        }

        if let err = envelope.error {
            throw OhyaRunnerError.commandFailed(
                code: err.code,
                message: err.message,
                hint: err.hint,
                exitCode: raw.exitCode
            )
        }

        guard allowed, let data = envelope.data else {
            throw OhyaRunnerError.commandFailed(
                code: "unexpected_exit",
                message: "ohya exited with status \(raw.exitCode)",
                hint: "",
                exitCode: raw.exitCode
            )
        }

        return CLIResponse(data: data, degraded: raw.exitCode == 3, exitCode: raw.exitCode)
    }

    public func runRaw(
        binaryPath: String,
        arguments: [String],
        timeout: TimeInterval = 30
    ) async throws -> (exitCode: Int32, stdout: Data, stderr: Data) {
        try await execute(binaryPath: binaryPath, arguments: arguments, timeout: timeout)
    }

    private func execute(
        binaryPath: String,
        arguments: [String],
        timeout: TimeInterval
    ) async throws -> (exitCode: Int32, stdout: Data, stderr: Data) {
        try await withCheckedThrowingContinuation { continuation in
            let process = Process()
            process.executableURL = URL(fileURLWithPath: binaryPath)
            process.arguments = arguments
            process.currentDirectoryURL = URL(fileURLWithPath: NSTemporaryDirectory())
            process.environment = scrubbedEnvironment()

            let stdoutPipe = Pipe()
            let stderrPipe = Pipe()
            process.standardOutput = stdoutPipe
            process.standardError = stderrPipe
            process.standardInput = FileHandle.nullDevice

            let state = LockedExecutionState()

            let timer = DispatchSource.makeTimerSource(queue: DispatchQueue.global(qos: .utility))
            timer.schedule(deadline: .now() + timeout)
            timer.setEventHandler {
                if state.markFinished() {
                    if process.isRunning {
                        process.terminate()
                    }
                    continuation.resume(throwing: OhyaRunnerError.timedOut(command: arguments, seconds: timeout))
                }
            }

            let stdoutBuffer = PipeCollector(pipe: stdoutPipe)
            let stderrBuffer = PipeCollector(pipe: stderrPipe)

            process.terminationHandler = { finished in
                timer.cancel()
                let out = stdoutBuffer.finish()
                let err = stderrBuffer.finish()
                if state.markFinished() {
                    continuation.resume(returning: (finished.terminationStatus, out, err))
                }
            }

            do {
                try process.run()
                timer.resume()
            } catch {
                timer.cancel()
                _ = stdoutBuffer.finish()
                _ = stderrBuffer.finish()
                if state.markFinished() {
                    continuation.resume(throwing: OhyaRunnerError.processLaunchFailed(error.localizedDescription))
                }
            }
        }
    }

    private func scrubbedEnvironment() -> [String: String] {
        let allowedKeys = [
            "HOME",
            "USER",
            "LOGNAME",
            "TMPDIR",
            "PATH",
            "LANG",
            "LC_ALL",
            "CODEX_HOME",
            "CLAUDE_CONFIG_DIR",
            "PI_CODING_AGENT_DIR",
            "PI_CODING_AGENT_SESSION_DIR"
        ]
        let current = ProcessInfo.processInfo.environment
        var filtered: [String: String] = [:]
        for key in allowedKeys {
            if let value = current[key] {
                filtered[key] = value
            }
        }
        return filtered
    }
}

private final class LockedExecutionState: @unchecked Sendable {
    private let lock = NSLock()
    private var finished = false

    func markFinished() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        if finished {
            return false
        }
        finished = true
        return true
    }
}

private final class PipeCollector: @unchecked Sendable {
    private let lock = NSLock()
    private let pipe: Pipe
    private var data = Data()

    init(pipe: Pipe) {
        self.pipe = pipe
        pipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let chunk = handle.availableData
            guard !chunk.isEmpty else {
                handle.readabilityHandler = nil
                return
            }
            self?.append(chunk)
        }
    }

    private func append(_ chunk: Data) {
        lock.lock()
        data.append(chunk)
        lock.unlock()
    }

    func finish() -> Data {
        pipe.fileHandleForReading.readabilityHandler = nil
        let trailing = pipe.fileHandleForReading.readDataToEndOfFile()
        lock.lock()
        if !trailing.isEmpty {
            data.append(trailing)
        }
        let result = data
        lock.unlock()
        return result
    }
}
