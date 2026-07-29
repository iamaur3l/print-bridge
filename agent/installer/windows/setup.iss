; PrintBridge Agent Inno Setup Script
#define MyAppName "PrintBridge Agent"
#define MyAppVersion "0.1.0"
#define MyAppPublisher "PrintBridge Open Source Project"
#define MyAppURL "https://github.com/printbridge/printbridge"
#define MyAppExeName "printbridge-agent.exe"

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
PrivilegesRequired=lowest

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "autostart"; Description: "Automatically start PrintBridge Agent on Windows login"; GroupDescription: "Startup Options:"

[Files]
Source: "..\..\build\windows-amd64\{#MyAppExeName}"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "PrintBridgeAgent"; ValueData: """{app}\{#MyAppExeName}"""; Tasks: autostart; Flags: uninsdeletevalue

[Run]
Filename: "{app}\{#MyAppExeName}"; Description: "{cm:LaunchProgram,{#StringChange(MyAppName, '&', '&&')}}"; Flags: nowait postinstall skipifsilent
