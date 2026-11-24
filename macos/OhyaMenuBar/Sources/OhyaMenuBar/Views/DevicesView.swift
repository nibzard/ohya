import AppKit
import OhyaMenuBarCore
import SwiftUI

struct DevicesView: View {
    let model: AppModel
    @State private var revokingDevice: PairedDevice?
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                if model.summary.isRemote {
                    joinedDeviceCard
                } else {
                    hubDevicesContent
                }
            }
            .padding(14)
        }
        .confirmationDialog(
            "Revoke device \(revokingDevice.map { DisplayFormat.shortID($0.id) } ?? "")?",
            isPresented: Binding(
                get: { revokingDevice != nil },
                set: { if !$0 { revokingDevice = nil } }
            ),
            titleVisibility: .visible
        ) {
            Button("Revoke Permanently", role: .destructive) {
                if let target = revokingDevice {
                    Task { await model.revokeDevice(target.id) }
                }
                revokingDevice = nil
            }
            Button("Cancel", role: .cancel) {
                revokingDevice = nil
            }
        } message: {
            Text("Revocation is final for this device identity. Its connections close immediately and re-admission requires a new device identity.")
        }
    }

    private var joinedDeviceCard: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label("Joined to a remote hub", systemImage: "network")
                .font(.subheadline.weight(.medium))

            if let transport = model.doctor?.transport {
                VStack(spacing: 5) {
                    row("Hub reachable", value: transport.hubAnswers.map { $0 ? "Yes" : "No" } ?? "Checking")
                    row("Path", value: transport.path.capitalized)
                    if let region = transport.relayRegion, !region.isEmpty {
                        row("Relay", value: region)
                    }
                    if let rtt = transport.roundTripMS {
                        row("Round trip", value: String(format: "%.0f ms", rtt))
                    }
                }
            } else {
                Text("Sessions upload automatically over the encrypted Tailcat tailnet.")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .textSelection(.enabled)
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(10)
        .ohyaGlassControl(cornerRadius: 10)
    }

    @ViewBuilder
    private var hubDevicesContent: some View {
        HStack {
            Text("Paired Devices")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            Spacer()
            Button {
                Task { await model.createDeviceInvite() }
            } label: {
                Label("Invite Device", systemImage: "plus")
                    .font(.caption.weight(.medium))
            }
            .buttonStyle(.bordered)
            .controlSize(.small)
            .disabled(model.resolvedBinaryPath == nil)
        }

        if let invite = model.activeInvite {
            inviteCard(invite)
        }

        if let err = model.devicesError {
            HStack(alignment: .top, spacing: 6) {
                Text(err)
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
                    .textSelection(.enabled)
                Spacer(minLength: 4)
                CopyFeedbackButton("Copy") {
                    model.copyText(err)
                }
            }
        }

        if model.isLoadingDevices && model.devicesData == nil {
            ProgressView()
                .controlSize(.small)
                .frame(maxWidth: .infinity)
                .padding(.vertical, 16)
        } else if let devices = model.devicesData?.devices, !devices.isEmpty {
            VStack(spacing: 0) {
                ForEach(Array(devices.enumerated()), id: \.element.id) { index, device in
                    deviceRow(device)
                    if index < devices.count - 1 {
                        Divider().opacity(0.35)
                    }
                }
            }
        } else {
            Text("No remote devices paired yet. Run ohya setup --hub on this Mac, then click Invite Device.")
                .font(.caption)
                .foregroundStyle(.secondary)
                .padding(.vertical, 12)
        }
    }

    private func inviteCard(_ invite: DeviceInviteData) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Label("One-time invitation", systemImage: "key.fill")
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(OhyaTheme.brandPurple)
                Spacer()
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    if let remaining = DisplayFormat.countdown(untilRFC3339: invite.expiresAt, now: context.date) {
                        Text(remaining)
                            .font(.caption.monospacedDigit().weight(.medium))
                            .foregroundStyle(remaining == "Expired" ? .red : .secondary)
                            .contentTransition(reduceMotion ? .identity : .numericText())
                    }
                }
            }

            Text("Code stays in memory only. Copying tags it concealed and clears your clipboard after 60s.")
                .font(.caption2)
                .foregroundStyle(.secondary)

            HStack(spacing: 8) {
                CopyFeedbackButton("Copy Code", copiedLabel: "Copied", systemImage: "lock.doc") {
                    model.copyInviteCode()
                }
                CopyFeedbackButton("Copy Install Cmd", copiedLabel: "Copied", systemImage: "terminal") {
                    model.copyJoinCommand()
                }
                Spacer()
            }
        }
        .padding(10)
        .ohyaGlassControl(cornerRadius: 10)
    }

    private func deviceRow(_ device: PairedDevice) -> some View {
        HStack(spacing: 8) {
            Circle()
                .fill(device.state == "paired" ? Color.green : Color.secondary.opacity(0.4))
                .frame(width: 6, height: 6)
            VStack(alignment: .leading, spacing: 2) {
                Text(DisplayFormat.shortID(device.id))
                    .font(.subheadline.monospaced())
                HStack(spacing: 4) {
                    Text(device.state)
                    if let path = device.lastPath, !path.isEmpty {
                        Text("·")
                        Text(path)
                    }
                }
                .font(.caption2)
                .foregroundStyle(.secondary)
            }
            .textSelection(.enabled)
            Spacer()
            if device.state == "paired" {
                Button("Revoke") {
                    NSApp.activate(ignoringOtherApps: true)
                    revokingDevice = device
                }
                .buttonStyle(.borderless)
                .font(.caption)
                .foregroundStyle(.red)
            }
        }
        .padding(.vertical, 6)
        .contextMenu {
            Button("Copy Device ID") {
                model.copyText(device.id, notice: "Device ID copied")
            }
        }
    }

    private func row(_ label: String, value: String) -> some View {
        HStack {
            Text(label)
                .font(.caption)
                .foregroundStyle(.secondary)
            Spacer()
            Text(value)
                .font(.caption.monospacedDigit())
        }
    }
}
