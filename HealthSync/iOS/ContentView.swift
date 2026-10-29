import SwiftUI

struct ContentView: View {
    @StateObject private var engine = SyncEngine.shared
    @StateObject private var client = SyncClient()
    @Environment(\.scenePhase) private var scenePhase
    @State private var enabledTypes = TypeSelection.allIDs.filter(TypeSelection.isEnabled).count

    var body: some View {
        NavigationStack {
            List {
                if !engine.authorized {
                    Section {
                        Button("Allow access to Health data") { Task { await engine.authorize() } }
                            .disabled(engine.state == .authorizing)
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
                            Task { await engine.sync(to: r, trigger: .manual) }
                        } label: {
                            HStack {
                                Label(r.name, systemImage: "desktopcomputer")
                                if r.name == engine.lastMacName {
                                    Spacer()
                                    Image(systemName: "checkmark").foregroundStyle(.secondary)
                                }
                            }
                        }
                        .disabled(!engine.authorized || engine.isRunning)
                    }
                }

                Section {
                    TextField("Mac address, e.g. macbook-2.local or 192.168.1.10:47800", text: $engine.manualAddress)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .keyboardType(.URL)
                    Button("Sync to this address") {
                        Task { await engine.syncToLastMac(trigger: .manual, deadline: nil) }
                    }
                    .disabled(!engine.authorized || engine.isRunning || engine.manualAddress.trimmingCharacters(in: .whitespaces).isEmpty)
                } header: {
                    Text("Or connect by address")
                } footer: {
                    Text("The Mac app shows its address. When this is filled in it's used for every sync instead of discovery, which also works on networks that block Bonjour. Port \(SyncProtocol.defaultPort) is assumed if you leave it out.")
                }

                Section {
                    Toggle("Sync automatically", isOn: $engine.autoSync)
                        .onChange(of: engine.autoSync) { _, on in
                            if on { BackgroundSync.start(store: engine.reader.store) } else { BackgroundSync.stop(store: engine.reader.store) }
                        }
                    if let name = engine.lastMacName {
                        LabeledContent("Syncs to", value: name)
                    }
                    if let date = engine.lastSynced {
                        LabeledContent("Last synced") {
                            VStack(alignment: .trailing) {
                                Text(date, format: .dateTime.day().month().hour().minute())
                                if let t = engine.lastTrigger { Text(label(for: t)).font(.caption).foregroundStyle(.secondary) }
                            }
                        }
                    }
                } footer: {
                    Text("When on, the app syncs to the last Mac you used as soon as you open it, and iOS also wakes it in the background when new Health data arrives (roughly hourly, only while the phone is unlocked). The Mac app has to be running for that to work; if it isn't, nothing is lost and the next sync catches up.")
                }

                Section {
                    NavigationLink {
                        DataTypesView(store: engine.reader.store) {
                            enabledTypes = TypeSelection.allIDs.filter(TypeSelection.isEnabled).count
                            if engine.autoSync { BackgroundSync.restart(store: engine.reader.store) }
                        }
                    } label: {
                        LabeledContent("Data types", value: "\(enabledTypes) of \(TypeSelection.allIDs.count) on")
                    }
                } footer: {
                    Text("Pick what gets synced. Switching a type off stops sending it; what was already sent stays on the receiver.")
                }

                Section {
                    Toggle("Send everything again", isOn: $engine.fullResync)
                } footer: {
                    Text("Normally only new or changed data is sent. Turn this on if you set up a new database on the Mac.")
                }

                Section("Status") { status }
            }
            .navigationTitle("Health Sync")
        }
        .onAppear { client.startBrowsing() }
        .onDisappear { client.stopBrowsing() }
        .onChange(of: scenePhase) { _, phase in
            if phase == .active { autoSyncOnOpen() }
        }
        .task { autoSyncOnOpen() }
    }

    private func autoSyncOnOpen() {
        guard engine.autoSync, engine.authorized, engine.hasTarget, !engine.isRunning else { return }
        if let last = engine.lastSynced, Date().timeIntervalSince(last) < 5 * 60 { return }
        Task { await engine.syncToLastMac(trigger: .launch, deadline: nil) }
    }

    private func label(for t: SyncEngine.Trigger) -> String {
        switch t {
        case .manual: return "started by you"
        case .launch: return "on opening the app"
        case .healthKit: return "in the background (new Health data)"
        case .backgroundRefresh: return "in the background (refresh)"
        }
    }

    @ViewBuilder private var status: some View {
        switch engine.state {
        case .idle: Text("Ready").foregroundStyle(.secondary)
        case .authorizing: Text("Waiting for Health permission…")
        case .syncing:
            VStack(alignment: .leading) {
                HStack { ProgressView(); Text("Reading \(engine.currentType)…") }
                Text("\(engine.sentCount.formatted()) items sent").font(.footnote).foregroundStyle(.secondary).monospacedDigit()
            }
        case .done(let n):
            Label("Done. \(n.formatted()) items sent.", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
        case .paused(let n):
            Label("Sent \(n.formatted()) items before iOS ended the background window; the rest goes next time.", systemImage: "pause.circle.fill")
                .foregroundStyle(.orange)
        case .failed(let m):
            Label(m, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red)
        }
    }
}
