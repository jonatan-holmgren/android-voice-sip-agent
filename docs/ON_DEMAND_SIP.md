# On-demand SIP gateway

On-demand mode keeps the existing cellular bridge and SIP stack, but creates them only after a wake request. The default remains the existing always-on mode. Incoming cellular calls are not taken over by this feature: carrier call forwarding continues independently.

## Request flow

1. An authenticated Asterisk dialplan requests a wake from the small Go service.
2. The server sends an FCM HTTP v1 high-priority data message containing only a request ID and expiry.
3. Android validates the delivery priority, request ID and expiry, then starts the existing special-use foreground service.
4. The service starts Shizuku setup and PJSIP on the existing serialized runtime worker. The bridge keeps the existing `X-VSA-Dial`, `X-VSA-SIMSLOT`, TLS/SRTP, SIM and emergency-number behavior.
5. After SIP registration and cellular/Shizuku readiness succeed, the app reports readiness for that request ID to the server over authenticated HTTPS. Asterisk waits on its own call channel for that one-shot readiness result and for its PJSIP contact to be present. It sends the INVITE only after both checks pass; the contact table remains authoritative for SIP reachability.
6. When the call ends, the app waits the configured grace period (default 60 seconds), unregisters and destroys the PJSUA library. The dormant state has no app SIP transport, SIP event loop, RTP worker, keepalive or app polling job.

FCM wakes availability only. The normal authenticated SIP account, Asterisk context and the app's current outbound-prefix/emergency checks authorize the cellular call.

## Android and Firebase setup

1. Create a Firebase project, add an Android app whose package name is `io.github.sparkcyf.voicesipagent`, and enable the Firebase Cloud Messaging API. The Gradle properties below are Firebase client identifiers; they are not the server service-account credential.
2. Put these values in the machine-local `~/.gradle/gradle.properties` (do not commit them):

   ```properties
   vsa.firebase.apiKey=YOUR_FIREBASE_WEB_API_KEY
   vsa.firebase.appId=1:PROJECT_NUMBER:android:APP_HASH
   vsa.firebase.projectId=YOUR_FIREBASE_PROJECT_ID
   vsa.firebase.senderId=YOUR_FIREBASE_PROJECT_NUMBER
   ```

   Rebuild and install the APK. An empty Firebase configuration is allowed for builds, but on-demand status will say Firebase configuration is missing and no FCM token will be registered.
3. Run `wake-server` behind an HTTPS reverse proxy. Keep the Go listener bound to localhost or a private network, restrict the public proxy to the required routes, and allow Asterisk to reach it. The app refuses non-HTTPS token registration and uses the platform TLS verifier.
4. Create a service account with the Firebase Cloud Messaging API send permission. Provide its JSON file to the server using `GOOGLE_APPLICATION_CREDENTIALS`; keep it outside the repository and readable only by the service account running the server. Do not bake this credential into the APK.
5. Configure the server environment with separate random values for `VSA_DEVICE_REGISTRATION_KEY` and `VSA_ASTERISK_API_KEY`, and set `VSA_DEVICE_STORE` to a private writable path. The device store is written mode `0600`; back it up only as a secret. Never expose the registration or wake route without HTTPS and bearer authentication.
6. Launch the app once, enable **On-demand SIP (FCM)**, enter the HTTPS wake-server origin (for example `https://wake.example.net`, without `/api/v1`), the device registration key, and an idle timeout from 15 to 600 seconds. The app registers its FCM token using an authenticated `PUT`; token rotation repeats registration. Copy the displayed installation ID into the Asterisk local secrets include.
7. Keep Shizuku enabled and the app configured for the intended SIP account. The first wake cannot prompt for Shizuku permission reliably from the background; grant/setup it from the app while visible first.

## Go wake service

Build and run from `wake-server/`:

```sh
go test ./...
go build -o vsa-wake .
```

Runtime configuration:

- `GOOGLE_APPLICATION_CREDENTIALS`: path to the service-account JSON file.
- `VSA_DEVICE_REGISTRATION_KEY`: bearer key used only by the Android client for token registration.
- `VSA_ASTERISK_API_KEY`: separate bearer key used only by Asterisk for wake requests.
- `VSA_DEVICE_STORE`: private JSON file path; defaults to `/var/lib/vsa-wake/devices.json`.
- `VSA_LISTEN_ADDR`: defaults to `127.0.0.1:8077`.

The API accepts `PUT /api/v1/devices/token` and `POST /api/v1/wake`. Wake requests are rate limited per installation and may include an `Idempotency-Key`; repeated requests return the same request ID during the deduplication window. The phone reports readiness with `POST /api/v1/wake/{request_id}/ready`, authenticated with the device-registration key; Asterisk reads `GET /api/v1/wake/{request_id}` with its separate key. The status endpoint returns `pending`, `ready`, or `failed` and expires the request. The app report says only that SIP registered and its local cellular/Shizuku prerequisites passed; it cannot authorize a call and does not replace Asterisk's PJSIP contact check. A stale/unregistered FCM token is removed and must be registered again by the app. The server logs neither the FCM token, bearer values nor call destinations.

The wake API acknowledges FCM submission, not gateway readiness. Asterisk waits for the app's readiness report and independently checks the registered PJSIP contact; it fails the call if either does not arrive by the configured dialplan timeout.

## Asterisk setup

Use the examples in [`asterisk/pjsip.conf.example`](../asterisk/pjsip.conf.example) and [`asterisk/extensions.conf.example`](../asterisk/extensions.conf.example). Give the authenticated Acrobits endpoint a restricted context that includes `[from-softphone]`. Install the Android gateway endpoint in PJSIP, and set the app to the same account, TLS transport and password. Keep the generated trunk secret and `VSA_WAKE_API_KEY` in root-owned Asterisk configuration, not in a public dialplan repository.

The sample dialplan sends the original dialed extension to the gateway and copies a valid `X-VSA-SIMSLOT` header. It first requests FCM, waits in that call's dialplan execution for the app's one-shot readiness result, and checks `${PJSIP_DIAL_CONTACTS(android-gw,android-gw-aor,${VSA_DEST})}` every second. The contact check is Asterisk's registered contact state. If FCM submission fails it hangs up with cause 38; if readiness or the contact does not arrive within 45 seconds it hangs up with cause 34. No SIP INVITE is sent before both checks pass.

Review the sample against the PBX's installed Asterisk release and existing authentication/context policy before deploying it. Keep call authorization in the authenticated Acrobits endpoint and the existing gateway prefix/emergency rules. The wake request contains no phone number and is not a call command.

## Android background limits

High-priority FCM attempts prompt delivery during Doze, but Google may downgrade high-priority data messages if they are not producing user-visible outcomes. Android 12+ restricts background foreground-service starts; receipt of an actually high-priority FCM message is an exemption, but a downgraded message is not. The receiver checks the delivered priority and refuses to start SIP for a downgraded message. The foreground-service start is also caught and shown in diagnostics if Android denies it. Android 14+ requires a valid foreground-service type and type permission; this app uses its declared `specialUse` type and does not claim microphone, camera, location or other while-in-use service types. OEM battery managers, lack of Google Play services, disabled notifications, force-stop, unavailable data, and FCM outages can still delay or prevent wake-up. A force-stopped app must be launched by the user again before FCM delivery resumes.

There is no periodic polling fallback. If wake fails, Asterisk receives a normal route failure after its timeout. The notification and app diagnostics show token registration, lifecycle state, SIP state, last wake result/latency, Shizuku/cellular readiness, and the failure detail.

## Physical-device validation

Automated JVM tests cannot prove FCM delivery, OEM process survival, Android service launch exemptions, SIP contact timing or cellular audio. Validate on the target phone and Android release:

1. Start with the app open; verify the FCM status is **Registered** and the wake server has the device ID.
2. Turn on on-demand mode. Confirm Asterisk reports no gateway contact, the app lifecycle reports `DORMANT`, and the app holds no foreground notification.
3. In a separate terminal, inspect sockets and wakeups while dormant:

   ```sh
   adb shell dumpsys activity services io.github.sparkcyf.voicesipagent
   adb shell dumpsys netstats | grep -i voicesip
   adb shell dumpsys batterystats --reset
   ```

   After a representative idle period, collect `adb shell dumpsys batterystats` and compare against always-on mode on the same network/device. Confirm there is no app UDP/TCP SIP socket and no PJSIP/RTP worker thread in a bugreport or debugger thread dump.
4. Put the phone into Doze with `adb shell dumpsys deviceidle force-idle`, place an authorized test call from Acrobits, and observe `adb logcat -s VoiceSipAgent`, the FCM request ID, the service notification, SIP registration, and Asterisk's PJSIP contact. Record wake latency from the app UI/log and Asterisk call logs. Restore with `adb shell dumpsys deviceidle unforce`.
5. Call with no network, Shizuku stopped, bad SIP credentials, a repeated request ID, an expired request and a high-priority message downgraded by FCM. Confirm each failure is surfaced and no INVITE is sent before the PJSIP contact is usable.
6. Complete a call, confirm the SIP contact unregisters after the selected idle timeout, and confirm the pjsua library and foreground service stop. Reboot and kill the app process to validate fresh token registration and cold-start wake.
7. Place an ordinary incoming cellular call while dormant and while the gateway is awake with no gateway SIP call. Confirm the gateway does not answer or alter it. Carrier call forwarding to the DID remains a separate carrier feature.

**Same-phone audio is a separate unresolved device issue.** If Acrobits and this gateway run on the same physical phone, concurrent local VoIP audio and a cellular call can conflict through Android audio focus, Telecom audio routing and vendor audio policy. FCM only wakes the gateway; it does not solve this conflict. Test an Acrobits call through the gateway on each target model/OS. Until verified, use Acrobits on another device for dependable simultaneous VoIP/cellular operation.

## Scope of a future embedded softphone

Adding a softphone is a substantial separate feature. The repository has a reusable `CallEndpoint` contract and a PJSUA2 trunk endpoint, but no user-facing SIP account/call UI, incoming-call presentation, local microphone/speaker routing, call controls, Telecom integration for VoIP calls, or softphone audio-focus policy. The existing PJSUA2 endpoint deliberately uses a null sound device and carries PCM only through the cellular bridge. A softphone would need its own endpoint/audio path and permission/notification/call lifecycle; it should not be implemented by changing the cellular bridge. It also would not remove the same-device audio-focus risk described above.
