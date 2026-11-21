import Darwin
import Foundation

public final class ObserveWatcher: @unchecked Sendable {
    private let queue = DispatchQueue(label: "com.nibzard.ohya.observe-watcher", qos: .utility)
    private var source: DispatchSourceFileSystemObject?
    private var fileDescriptor: Int32 = -1
    private var watchedPath: String?

    public init() {}

    public func watch(directory: String, onChange: @escaping @Sendable () -> Void) {
        queue.async { [weak self] in
            guard let self else { return }
            if self.watchedPath == directory, self.source != nil {
                return
            }
            self.stopLocked()
            let fd = open(directory, O_EVTONLY)
            guard fd >= 0 else {
                self.watchedPath = nil
                return
            }
            let dispatchSource = DispatchSource.makeFileSystemObjectSource(
                fileDescriptor: fd,
                eventMask: [.write, .rename, .delete, .extend],
                queue: self.queue
            )
            dispatchSource.setEventHandler {
                onChange()
            }
            dispatchSource.setCancelHandler {
                close(fd)
            }
            self.fileDescriptor = fd
            self.source = dispatchSource
            self.watchedPath = directory
            dispatchSource.resume()
        }
    }

    public func stop() {
        queue.async { [weak self] in
            self?.stopLocked()
        }
    }

    private func stopLocked() {
        if let existing = source {
            existing.cancel()
            source = nil
        } else if fileDescriptor >= 0 {
            close(fileDescriptor)
        }
        fileDescriptor = -1
        watchedPath = nil
    }

    deinit {
        if let existing = source {
            existing.cancel()
        } else if fileDescriptor >= 0 {
            close(fileDescriptor)
        }
    }
}
