import SwiftUI
import UIKit

@MainActor
final class SyncViewModel: ObservableObject {
    enum State: Equatable { case idle, authorizing, syncing, done(Int), failed(String) }

    @Published var state: State = .idle
    @Published var currentType = ""
    @Published var sentCount = 0
    @Published var fullResync = false
    @Published var authorized = UserDefaults.standard.bool(forKey: "authorized")

    private let reader = HealthReader()

    func authorize() async {
        state = .authorizing
        do {
            try await reader.requestAuthorization()
            authorized = true
            UserDefaults.standard.set(true, forKey: "authorized")
            state = .idle
        } catch {
            state = .failed(error.localizedDescription)
        }
    }

    func sync(to receiver: SyncClient.Receiver, using client: SyncClient) async {
        state = .syncing
        sentCount = 0
        reader.fullResync = fullResync
        UIApplication.shared.isIdleTimerDisabled = true
        defer { UIApplication.shared.isIdleTimerDisabled = false }
        do {
            let connection = try await client.connect(to: receiver)
            try await connection.send(.hello(.init(protocolVersion: SyncProtocol.version,
                                                   deviceName: UIDevice.current.name,
                                                   systemVersion: UIDevice.current.systemVersion,
                                                   fullResync: fullResync)))
            var n = 0
            try await reader.readAll(progress: { [weak self] label in
                Task { @MainActor in self?.currentType = label }
            }, emit: { [weak self] message in
                try await connection.send(message)
                n += 1
                if n % 2_000 == 0 { await MainActor.run { self?.sentCount = n } }
            })
            try await connection.send(.done(totalSent: n))
            try await connection.flush()
            let acked = try await connection.waitForAck()
            connection.close()
            sentCount = n
            fullResync = false
            state = .done(acked)
        } catch {
            state = .failed(error.localizedDescription)
        }
    }
}

struct ContentView: View {
    @StateObject private var vm = SyncViewModel()
    @StateObject private var client = SyncClient()

    var body: some View {
        NavigationStack {
            List {
                if !vm.authorized {
                    Section {
                        Button("Allow access to Health data") { Task { await vm.authorize() } }
                            .disabled(vm.state == .authorizing)
                    } footer: {
                        Text("Turn on every category on the next screen so the full history can be exported. Nothing leaves your phone except over your own Wi-Fi to the Mac you pick below.")
                    }
                }

                Section("Macs on this network") {
                    if client.receivers.isEmpty {
                        HStack { ProgressView(); Text("Looking for Health Sync on your Mac…").foregroundStyle(.secondary) }
                    }
                    ForEach(client.receivers) { r in
                        Button {
                            Task { await vm.sync(to: r, using: client) }
                        } label: {
                            Label(r.name, systemImage: "desktopcomputer")
                        }
                        .disabled(!vm.authorized || vm.state == .syncing)
                    }
                }

                Section {
                    Toggle("Send everything again", isOn: $vm.fullResync)
                } footer: {
                    Text("Normally only new or changed data is sent. Turn this on if you set up a new database on the Mac.")
                }

                Section("Status") { status }
            }
            .navigationTitle("Health Sync")
        }
        .onAppear { client.startBrowsing() }
        .onDisappear { client.stopBrowsing() }
    }

    @ViewBuilder private var status: some View {
        switch vm.state {
        case .idle: Text("Ready").foregroundStyle(.secondary)
        case .authorizing: Text("Waiting for Health permission…")
        case .syncing:
            VStack(alignment: .leading) {
                HStack { ProgressView(); Text("Reading \(vm.currentType)…") }
                Text("\(vm.sentCount.formatted()) items sent").font(.footnote).foregroundStyle(.secondary).monospacedDigit()
            }
        case .done(let n): Label("Done. The Mac confirmed \(n.formatted()) items.", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
        case .failed(let m): Label(m, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
        }
    }
}
