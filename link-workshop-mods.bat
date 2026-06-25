@echo off
setlocal EnableExtensions EnableDelayedExpansion

rem Creates @workshop-id junctions for downloaded Arma 3 Workshop mods and copies .bikey files.
rem Usage:
rem   link-workshop-mods.bat [server-root]
rem You can run it from the server root or from steamapps\workshop\content\107410.

for %%I in ("%CD%") do set "CURNAME=%%~nxI"
for %%I in ("%CD%\..") do set "PARENTNAME=%%~nxI"
for %%I in ("%CD%\..\..") do set "GRANDPARENTNAME=%%~nxI"
for %%I in ("%CD%\..\..\..") do set "GREATGRANDPARENTNAME=%%~nxI"

if not "%~1"=="" (
    set "ROOT=%~1"
) else if exist "%CD%\steamapps\workshop\content\107410\" (
    set "ROOT=%CD%"
) else if /I "%CURNAME%"=="107410" if /I "%PARENTNAME%"=="content" if /I "%GRANDPARENTNAME%"=="workshop" if /I "%GREATGRANDPARENTNAME%"=="steamapps" (
    for %%I in ("%CD%\..\..\..\..") do set "ROOT=%%~fI"
) else (
    echo Could not detect server root.
    echo Run from the server root, from steamapps\workshop\content\107410, or pass root as argument.
    echo Example: link-workshop-mods.bat "C:\Pterodactyl\servers\d2b39347-25ab-47cb-b4e3-fee2dbe88a12"
    exit /b 1
)

set "WORKSHOP=%ROOT%\steamapps\workshop\content\107410"
set "KEYS=%ROOT%\keys"

if not exist "%WORKSHOP%\" (
    echo Workshop directory not found: "%WORKSHOP%"
    exit /b 1
)

if not exist "%KEYS%\" mkdir "%KEYS%" >nul 2>&1

set /a LINKED=0
set /a SKIPPED=0
set /a KEYS_COPIED=0

for /D %%D in ("%WORKSHOP%\*") do (
    set "ID=%%~nxD"
    set "SRC=%%~fD"
    set "TARGET=%ROOT%\@!ID!"

    if exist "!TARGET!\" (
        fsutil reparsepoint query "!TARGET!" >nul 2>&1
        if errorlevel 1 (
            echo SKIP @!ID!: target exists and is not a junction: "!TARGET!"
            set /a SKIPPED+=1
        ) else (
            rmdir "!TARGET!" >nul 2>&1
            mklink /J "!TARGET!" "!SRC!" >nul
            if errorlevel 1 (
                echo FAIL @!ID!: could not create junction
                set /a SKIPPED+=1
            ) else (
                echo LINK @!ID! -^> "!SRC!"
                set /a LINKED+=1
            )
        )
    ) else (
        mklink /J "!TARGET!" "!SRC!" >nul
        if errorlevel 1 (
            echo FAIL @!ID!: could not create junction
            set /a SKIPPED+=1
        ) else (
            echo LINK @!ID! -^> "!SRC!"
            set /a LINKED+=1
        )
    )

    for /R "!SRC!" %%K in (*.bikey) do (
        copy /Y "%%~fK" "%KEYS%\%%~nxK" >nul
        if not errorlevel 1 set /a KEYS_COPIED+=1
    )
)

echo.
echo Done. Linked: %LINKED%, skipped: %SKIPPED%, bikeys copied: %KEYS_COPIED%
echo Root: "%ROOT%"
echo Keys: "%KEYS%"

endlocal
