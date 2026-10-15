export namespace model {
	
	export class ClipContent {
	    Kind: string;
	    Content: string;
	    PlainText: string;
	    Files: string[];
	    ImagePNG: number[];
	    ImageW: number;
	    ImageH: number;
	    Preview: string;
	    Hash: string;
	    SourceApp: string;
	    SourceTitle: string;
	    ByteSize: number;
	
	    static createFrom(source: any = {}) {
	        return new ClipContent(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Kind = source["Kind"];
	        this.Content = source["Content"];
	        this.PlainText = source["PlainText"];
	        this.Files = source["Files"];
	        this.ImagePNG = source["ImagePNG"];
	        this.ImageW = source["ImageW"];
	        this.ImageH = source["ImageH"];
	        this.Preview = source["Preview"];
	        this.Hash = source["Hash"];
	        this.SourceApp = source["SourceApp"];
	        this.SourceTitle = source["SourceTitle"];
	        this.ByteSize = source["ByteSize"];
	    }
	}
	export class Filter {
	    keyword: string;
	    kind: string;
	    pinboardId: string;
	    sourceApp: string;
	    since: number;
	    until: number;
	    limit: number;
	    offset: number;
	
	    static createFrom(source: any = {}) {
	        return new Filter(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.keyword = source["keyword"];
	        this.kind = source["kind"];
	        this.pinboardId = source["pinboardId"];
	        this.sourceApp = source["sourceApp"];
	        this.since = source["since"];
	        this.until = source["until"];
	        this.limit = source["limit"];
	        this.offset = source["offset"];
	    }
	}
	export class Item {
	    id: string;
	    kind: string;
	    content: string;
	    plainText: string;
	    preview: string;
	    blobPath: string;
	    thumbPath: string;
	    sourceApp: string;
	    sourceTitle: string;
	    hash: string;
	    pinned: boolean;
	    pinboardId: string;
	    // Go type: time
	    createdAt: any;
	    byteSize: number;
	
	    static createFrom(source: any = {}) {
	        return new Item(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.kind = source["kind"];
	        this.content = source["content"];
	        this.plainText = source["plainText"];
	        this.preview = source["preview"];
	        this.blobPath = source["blobPath"];
	        this.thumbPath = source["thumbPath"];
	        this.sourceApp = source["sourceApp"];
	        this.sourceTitle = source["sourceTitle"];
	        this.hash = source["hash"];
	        this.pinned = source["pinned"];
	        this.pinboardId = source["pinboardId"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.byteSize = source["byteSize"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Pinboard {
	    id: string;
	    name: string;
	    sortOrder: number;
	    // Go type: time
	    createdAt: any;
	    itemCount: number;
	
	    static createFrom(source: any = {}) {
	        return new Pinboard(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.sortOrder = source["sortOrder"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.itemCount = source["itemCount"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Settings {
	    hotkey: string;
	    maxItems: number;
	    excludedApps: string[];
	    autoStart: boolean;
	    pauseCapture: boolean;
	    pastePlain: boolean;
	    panelHeight: number;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.hotkey = source["hotkey"];
	        this.maxItems = source["maxItems"];
	        this.excludedApps = source["excludedApps"];
	        this.autoStart = source["autoStart"];
	        this.pauseCapture = source["pauseCapture"];
	        this.pastePlain = source["pastePlain"];
	        this.panelHeight = source["panelHeight"];
	    }
	}

}

