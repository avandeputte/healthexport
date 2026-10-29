import Foundation
import Network
import UIKit

/// The one place a sync runs from, whether the user tapped a Mac, the app was opened, HealthKit woke us in
/// the background, or iOS granted a background refresh. Only one sync runs at a time.
@MainActor
final class SyncEngine: ObservableObject {
    static let shared = SyncEngine()

    enum State: Equatable { case idle, authorizing, syncing, done(Int), paused(Int), failed(String) }
    enum Trigger: String { case manual, launch, healthKit, backgroundRefresh }

    @Published var state: State = .idle
    @Published var currentType = ""
    @Published var sentCount = 0
    @Published var fullResync = false
    @Published private(set) var isRunning = false
    @Published var authorized = UserDefaults.standard.bool(forKey: "authorized")
    @Published var autoSync = UserDefaults.standard.object(forKey: "autoSync") as? Bool ?? true {
        didSet { UserDefaults.standard.set(autoSync, forKey: "autoSync") }
    }
    @Published private(set) var lastMacName: String? = UserDefaults.standard.string(forKey: "lastMac")
    /// Optional "host" or "host:port" typed by the user; tried before Bonjour when set.
    @Published var manualAddress: String = UserDefaults.standard.string(forKey: "manualAddress") ?? "" {
        didSet { UserDefaults.standard.set(manualAddress, forKey: "manualAddress") }
    }
    @Published private(set) var lastSynced: Date? = UserDefaults.standard.object(forKey: "lastSynced") as? Date
    @Published private(set) var lastTrigger: Trigger? = UserDefaults.standard.string(forKey: "lastTrigger").flatMap(Trigger.init)
    @Published private(set) var lastBackgroundAttempt: Date?

    let reader = HealthReader()

    func authorize() async {
        state = .authorizing
        do {
            try await reader.requestAuthorization()
            authorized = true
            UserDefaults.standard.set(true, forKey: "authorized")
            state = .idle
            BackgroundSync.start(store: reader.store)
        } catch {
            state = .failed(error.localizedDescription)
        }
    }

    /// Sync to a Mac found by browsing.
    func sync(to receiver: SyncClient.Receiver, trigger: Trigger) async {
        await run(macName: receiver.name, endpoint: receiver.endpoint, trigger: trigger, deadline: nil)
    }

    /// Whether an automatic sync has somewhere to go: a typed-in address or a remembered Mac.
    var hasTarget: Bool { manualEndpoint != nil || lastMacName != nil }

    private var manualEndpoint: NWEndpoint? {
        let text = manualAddress.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return nil }
        var host = text, port = SyncProtocol.defaultPort
        if let colon = text.lastIndex(of: ":"), !text.hasPrefix("["), text.filter({ $0 == ":" }).count == 1,
           let p = UInt16(text[text.index(after: colon)...]) {
            host = String(text[..<colon]); port = p
        }
        return .hostPort(host: NWEndpoint.Host(host), port: NWEndpoint.Port(rawValue: port) ?? .any)
    }

    /// Sync without browsing: to the typed-in address if there is one, else to the last Mac by its Bonjour name.
    func syncToLastMac(trigger: Trigger, deadline: Date?) async {
        if let endpoint = manualEndpoint {
            await run(macName: manualAddress.trimmingCharacters(in: .whitespacesAndNewlines), endpoint: endpoint, trigger: trigger, deadline: deadline)
        } else if let name = lastMacName {
            let endpoint = NWEndpoint.service(name: name, type: SyncProtocol.bonjourType, domain: "local.", interface: nil)
            await run(macName: name, endpoint: endpoint, trigger: trigger, deadline: deadline)
        }
    }

    /// Called from HealthKit observer wakes and background refresh. Coalesces the burst of observer callbacks
    /// that arrive together, and keeps inside the ~30 s iOS gives a background wake.
    func backgroundSync(trigger: Trigger) async {
        guard autoSync, authorized, hasTarget, !isRunning else { return }
        if let last = lastBackgroundAttempt, Date().timeIntervalSince(last) < 10 * 60 { return }
        lastBackgroundAttempt = Date()
        let app = UIApplication.shared
        var bgTask = UIBackgroundTaskIdentifier.invalid
        bgTask = app.beginBackgroundTask(withName: "HealthSync") { app.endBackgroundTask(bgTask) }
        await syncToLastMac(trigger: trigger, deadline: Date().addingTimeInterval(22))
        app.endBackgroundTask(bgTask)
    }

    private func run(macName: String, endpoint: NWEndpoint, trigger: Trigger, deadline: Date?) async {
        guard !isRunning else { return }
        isRunning = true
        defer { isRunning = false }
        state = .syncing
        sentCount = 0
        currentType = ""
        reader.fullResync = fullResync
        let foreground = trigger == .manual || trigger == .launch
        if foreground { UIApplication.shared.isIdleTimerDisabled = true }
        defer { if foreground { UIApplication.shared.isIdleTimerDisabled = false } }

        var connection: SyncClient.Connection?
        do {
            let c = try await SyncClient.Connection.open(to: endpoint)
            connection = c
            try await c.send(.hello(.init(protocolVersion: SyncProtocol.version,
                                          deviceName: UIDevice.current.name,
                                          systemVersion: UIDevice.current.systemVersion,
                                          fullResync: fullResync)))
            var n = 0
            var ranOutOfTime = false
            do {
                try await reader.readAll(progress: { [weak self] label in
                    Task { @MainActor in self?.currentType = label }
                }, emit: { [weak self] message in
                    if let deadline, Date() > deadline { throw SyncError.outOfTime }
                    try await c.send(message)
                    n += 1
                    if n % 2_000 == 0 { await MainActor.run { self?.sentCount = n } }
                })
            } catch SyncError.outOfTime {
                // Anchors for finished batches are saved, so the next run picks up where this one stopped.
                ranOutOfTime = true
            }
            try await c.send(.done(totalSent: n))
            try await c.flush()
            _ = try await c.waitForAck()
            c.close()
            sentCount = n
            fullResync = false
            remember(macName: macName, trigger: trigger)
            state = ranOutOfTime ? .paused(n) : .done(n)
        } catch {
            connection?.close()
            state = .failed(error.localizedDescription)
        }
        BackgroundSync.scheduleRefresh()
    }

    private func remember(macName: String, trigger: Trigger) {
        lastMacName = macName
        lastSynced = Date()
        lastTrigger = trigger
        UserDefaults.standard.set(macName, forKey: "lastMac")
        UserDefaults.standard.set(lastSynced, forKey: "lastSynced")
        UserDefaults.standard.set(trigger.rawValue, forKey: "lastTrigger")
    }

    func forgetMac() {
        lastMacName = nil
        UserDefaults.standard.removeObject(forKey: "lastMac")
    }
}
