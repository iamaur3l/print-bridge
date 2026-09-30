; PrintBridge Agent Inno Setup Script
;
; The agent is installed as a Windows service: it starts with the machine, depends on
; the print spooler, and restarts itself if it fails. That is what lets a till be
; rebooted without anyone logging in.

#define MyAppName "PrintBridge Agent"
#define MyAppVersion "0.1.0"
#define MyAppPublisher "PrintBridge Open Source Project"
#define MyAppURL "https://github.com/iamaur3l/print-bridge"
#define MyAppExeName "printbridge-agent.exe"
#define ServiceName "printbridge-agent"

[Setup]
AppId={{D37E88A1-4B9F-4D2A-9A8C-311B9E77F41E}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
AppPublisherURL={#MyAppURL}
AppSupportURL={#MyAppURL}
AppUpdatesURL={#MyAppURL}
DefaultDirName={autopf}\PrintBridge
DefaultGroupName={#MyAppName}
DisableProgramGroupPage=yes
OutputDir=..\..\build\installer
OutputBaseFilename=PrintBridgeAgentSetup
Compression=lzma
SolidCompression=yes
WizardStyle=modern
; Registering a service needs administrator rights.
PrivilegesRequired=admin

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "startservice"; Description: "Start the PrintBridge service now"; GroupDescription: "Startup:"; Flags: checkedonce

[Files]
Source: "..\..\build\windows-amd64\{#MyAppExeName}"; DestDir: "{app}"; Flags: ignoreversion

[Dirs]
; A service runs as LocalSystem, so its database and configuration live in
; %ProgramData%\PrintBridge. Users get modify rights there so the same agent can also
; be run from a console for troubleshooting.
Name: "{commonappdata}\PrintBridge"; Permissions: users-modify

[Icons]
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autoprograms}\PrintBridge Dashboard"; Filename: "{code:GetDashboardURL}"

[Run]
Filename: "{app}\{#MyAppExeName}"; Parameters: "-service install"; Flags: runhidden waituntilterminated; StatusMsg: "Registering the PrintBridge service..."
Filename: "{app}\{#MyAppExeName}"; Parameters: "-service start"; Tasks: startservice; Flags: runhidden waituntilterminated; StatusMsg: "Starting the PrintBridge service..."
Filename: "{code:GetDashboardURL}"; Description: "Open the PrintBridge dashboard"; Flags: shellexec postinstall nowait skipifsilent

[UninstallRun]
Filename: "{app}\{#MyAppExeName}"; Parameters: "-service uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "UninstallPrintBridgeService"

[Code]
function GetDashboardURL(Param: string): string;
begin
  Result := 'http://localhost:9567/dashboard';
end;

