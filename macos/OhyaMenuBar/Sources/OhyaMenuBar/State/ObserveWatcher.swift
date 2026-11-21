import Darwin
import Foundation

public final class ObserveWatcher: @unchecked Sendable {
    private let queue = DispatchQueue(label: "com.nibzard.ohya.observe-watcher", qos: .utility)
    private var source: DispatchSourceFileSystemObject?
    private var watchedPath: String?

    public init() {}

    public func watch(directory: String, onChange: @escaping @Sendable () -> Void) {
        queue.async { [weak self] in
            guard let self else { return }
            if self.watchedPath == directory, self.source != nil {
                return
            }
            self.source?.cancel()
            self.source = nil
            let fd = open(directory, O_EVTONLY)
            guard fd >= 0 else { return }
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
            self.source = dispatchSource
            self.watchedPath = directory
            dispatchSource.resume()
        }
    }

    deinit {
        source?.cancel()
    }
}
