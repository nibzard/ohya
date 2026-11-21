import AppKit
import OhyaMenuBarCore
import SwiftUI

@main
struct OhyaMenuBarApp: App {
    @State private var model = AppModel()

    var body: some Scene {
        MenuBarExtra {
            PopoverRootView(model: model)
                .frame(width: OhyaTheme.popoverWidth)
                .onAppear {
                    model.setPopoverVisible(true)
                }
                .onDisappear {
                    model.setPopoverVisible(false)
                }
        } label: {
            Image(nsImage: MenuBarIconRenderer.render(state: model.summary.iconState))
                .accessibilityLabel(model.summary.iconState.accessibilityLabel)
        }
        .menuBarExtraStyle(.window)

        Window("ohya Settings", id: "settings") {
            SettingsView(model: model)
        }
        .windowResizability(.contentSize)
        .defaultPosition(.center)
    }
}
