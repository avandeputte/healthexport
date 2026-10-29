import SwiftUI

@main
struct HealthSyncMacApp: App {
    @StateObject private var model = ReceiverModel()

    var body: some Scene {
        WindowGroup("Health Sync") {
            ContentView()
                .environmentObject(model)
                .frame(minWidth: 620, minHeight: 440)
        }
        .commands {
            CommandGroup(replacing: .newItem) {}
        }
    }
}
