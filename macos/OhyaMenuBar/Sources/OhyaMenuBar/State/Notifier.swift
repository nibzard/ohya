import Foundation
import OhyaMenuBarCore
import UserNotifications

@MainActor
final class Notifier {
    private var lastSentAt: [String: Date] = [:]
    private let cooldown: TimeInterval = 600

    private var canUseUserNotifications: Bool {
        Bundle.main.bundleIdentifier != nil && Bundle.main.bundlePath.hasSuffix(".app")
    }

    func requestAuthorizationIfNeeded() async -> Bool {
        guard canUseUserNotifications else { return false }
        let center = UNUserNotificationCenter.current()
        let status = await withCheckedContinuation { (continuation: CheckedContinuation<UNAuthorizationStatus, Never>) in
            center.getNotificationSettings { settings in
                continuation.resume(returning: settings.authorizationStatus)
            }
        }
        switch status {
        case .authorized, .provisional:
            return true
        case .notDetermined:
            return (try? await center.requestAuthorization(options: [.alert, .sound])) ?? false
        default:
            return false
        }
    }

    func send(_ alert: TransitionAlert, enabled: Bool) {
        guard enabled, canUseUserNotifications else { return }
        let now = Date()
        if let last = lastSentAt[alert.identifier], now.timeIntervalSince(last) < cooldown {
            return
        }
        lastSentAt[alert.identifier] = now

        let content = UNMutableNotificationContent()
        content.title = alert.title
        content.body = alert.body
        content.sound = .default

        let request = UNNotificationRequest(
            identifier: alert.identifier,
            content: content,
            trigger: nil
        )
        UNUserNotificationCenter.current().add(request)
    }
}
