import { SolarReceptionServer } from 'file:///D:/Documents/Zab/Prism/.worktrees/eleven-local-20261010-voice-controls/.cache/solar-update/source/dist/server/index.mjs';
import { spawn } from 'node:child_process';
const resources = Object.fromEntries(['orion/state','solar/generations','solar/sessions'].map(k=>[k,{type:'orion.state/1',initial:{}}]));
for(const key of ['solar/lane-a','solar/lane-b']) resources[key]={type:'solar.lsml/1',initial:null};
const node = new SolarReceptionServer({binaryPath:'D:/Documents/Zab/Prism/.worktrees/eleven-local-20261010-voice-controls/.cache/solar-update/source/dist/native/win32-x64/lsdpd.exe',origins:['http://127.0.0.1:8099'],resources});
try {
 const connection=await node.start();
 const child=spawn('go',['test','./internal/api','-run','^(TestLaneCommitNative|TestSceneControlFailedSelectionDoesNotLoopNative)$','-v','-count=1'],{env:{...process.env,ORION_TEST_PRIVATE_LANE_ADDRESS:connection.address},stdio:'inherit'});
 const code=await new Promise((resolve,reject)=>{child.on('error',reject);child.on('exit',resolve)});
 if(code!==0) throw new Error(`Go native test exit ${code}`);
} finally {await node.stop();}
